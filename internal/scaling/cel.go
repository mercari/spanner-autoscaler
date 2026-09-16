/*

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package scaling

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/cel-go/cel"
	"k8s.io/apimachinery/pkg/util/intstr"

	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
)

// celCostLimit bounds the runtime evaluation cost of one expression. The
// expressions this package evaluates are simple comparisons over a few dozen
// scalar variables, so any evaluation approaching this limit is a bug or an
// adversarial expression; both should fail rather than stall the reconcile.
const celCostLimit = 1_000_000

// ErrWindowDataNotReady is wrapped by evaluation errors reported while
// status.currentCPUWindowMetrics does not yet cover every window declared in
// spec.scaleConfig.metricWindows (right after resource creation, controller
// restart with a young instance, or a metrics ingestion gap). CEL evaluation
// is skipped fail-safe until the data catches up.
var ErrWindowDataNotReady = errors.New("metric window aggregates not yet available")

// celEnvCache memoizes CEL environments keyed by the (metric flags, windows)
// pair they were derived from, and celProgramCache memoizes compiled
// programs keyed by environment plus expression — the same never-evicted
// pattern as the cron parse cache: the key space is the set of expressions
// appearing in specs, which is small and stable, and compiled programs are
// immutable and safe for concurrent use.
var (
	celEnvCache     sync.Map // string -> *cel.Env
	celProgramCache sync.Map // string -> cel.Program
)

// celMetricSegments maps each CPU metric type to its CEL variable name
// segment (cpu.<segment>, target.<segment>, cpu.<segment>.min15m, ...).
var celMetricSegments = []struct {
	flag    spannerv1beta1.CPUMetricFlags
	metric  spannerv1beta1.CPUMetricType
	segment string
}{
	{spannerv1beta1.CPUMetricFlagHighPriority, spannerv1beta1.CPUMetricTypeHighPriority, "highPriority"},
	{spannerv1beta1.CPUMetricFlagTotal, spannerv1beta1.CPUMetricTypeTotal, "total"},
}

// windowAggregations are the window aggregation kinds: the CEL variable name
// segment (cpu.<metric>.<name><window>) and how to read the value from a
// status entry. Adding a kind here extends the variable declarations, the
// webhook validation, and the runtime bindings together; only the
// CPUWindowMetric status struct and its two producers (the metrics client and
// the simulator's windowState) need a matching field.
var windowAggregations = []struct {
	name  string
	value func(spannerv1beta1.CPUWindowMetric) int
}{
	{"min", func(wm spannerv1beta1.CPUWindowMetric) int { return wm.Min }},
	{"avg", func(wm spannerv1beta1.CPUWindowMetric) int { return wm.Avg }},
	{"max", func(wm spannerv1beta1.CPUWindowMetric) int { return wm.Max }},
}

func celEnvKey(flags spannerv1beta1.CPUMetricFlags, windows []string) string {
	return fmt.Sprintf("%d|%s", flags, strings.Join(windows, ","))
}

// celEnv returns the CEL environment for the given metric flags and
// (already validated) metric windows. Every variable is declared explicitly
// with its qualified name, so an unknown or misspelled variable — including a
// window that is not declared in metricWindows — fails at compile time, which
// the webhook surfaces at admission.
func celEnv(flags spannerv1beta1.CPUMetricFlags, windows []string) (*cel.Env, error) {
	key := celEnvKey(flags, windows)
	if cached, ok := celEnvCache.Load(key); ok {
		return cached.(*cel.Env), nil
	}

	opts := []cel.EnvOption{
		// Let integer variables compare naturally against double literals
		// (e.g. cpu.total.avg1h > 32.5).
		cel.CrossTypeNumericComparisons(true),
		cel.Variable("current", cel.IntType),
		cel.Variable("desired", cel.IntType),
		cel.Variable("minPU", cel.IntType),
		cel.Variable("maxPU", cel.IntType),
		cel.Variable("now", cel.TimestampType),
		cel.Variable("lastScaleTime", cel.TimestampType),
		cel.Variable("activeSchedules", cel.ListType(cel.StringType)),
	}
	for _, m := range celMetricSegments {
		if flags&m.flag == 0 {
			continue
		}
		opts = append(opts,
			cel.Variable("cpu."+m.segment, cel.IntType),
			cel.Variable("target."+m.segment, cel.IntType),
		)
		for _, w := range windows {
			for _, agg := range windowAggregations {
				opts = append(opts, cel.Variable(fmt.Sprintf("cpu.%s.%s%s", m.segment, agg.name, w), cel.IntType))
			}
		}
	}

	env, err := cel.NewEnv(opts...)
	if err != nil {
		return nil, fmt.Errorf("creating CEL environment: %w", err)
	}
	celEnvCache.Store(key, env)
	return env, nil
}

// compiledProgram compiles expr against the environment derived from flags
// and windows, verifying that it evaluates to a boolean. Compiled programs
// are cached; compile errors are not (they are rare spec typos).
func compiledProgram(flags spannerv1beta1.CPUMetricFlags, windows []string, expr string) (cel.Program, error) {
	key := celEnvKey(flags, windows) + "\x00" + expr
	if cached, ok := celProgramCache.Load(key); ok {
		return cached.(cel.Program), nil
	}

	env, err := celEnv(flags, windows)
	if err != nil {
		return nil, err
	}
	ast, iss := env.Compile(expr)
	if iss.Err() != nil {
		return nil, fmt.Errorf("compiling CEL expression: %w", iss.Err())
	}
	if !ast.OutputType().IsExactType(cel.BoolType) {
		return nil, fmt.Errorf("CEL expression must evaluate to a boolean, got %s", ast.OutputType())
	}
	prg, err := env.Program(ast, cel.EvalOptions(cel.OptOptimize), cel.CostLimit(celCostLimit))
	if err != nil {
		return nil, fmt.Errorf("building CEL program: %w", err)
	}
	celProgramCache.Store(key, prg)
	return prg, nil
}

// CompileCondition compiles a spec CEL expression (a scalingRules[].when or a
// scale-up/scale-down condition) against the variable set derived from the
// configured CPU metrics and metric windows, verifying it evaluates to a
// boolean. The admission webhook calls this so that invalid expressions are
// rejected at apply time; because it shares the compilation (and cache) with
// runtime evaluation, validation and execution cannot diverge.
func CompileCondition(flags spannerv1beta1.CPUMetricFlags, windows []string, expr string) error {
	_, err := compiledProgram(flags, windows, expr)
	return err
}

// celActivation builds the CEL variable bindings from the resource state.
// builtinDesired is bound to `desired`: for trigger rules it is the built-in
// logic's desired PU, for gates it is the desired PU about to be applied.
// It returns an error wrapping ErrWindowDataNotReady when any declared
// (metric, window) aggregate is missing from status, so callers skip
// evaluation fail-safe instead of evaluating against stale zeros.
func celActivation(sa *spannerv1beta1.SpannerAutoscaler, builtinDesired int, now time.Time) (map[string]any, error) {
	flags := sa.Spec.ScaleConfig.TargetCPUUtilization.ActiveMetricFlags()
	windows := ValidMetricWindows(sa.Spec.ScaleConfig.MetricWindows)
	minPU, maxPU := effectiveRange(sa)

	scheduleNames := make([]string, 0, len(sa.Status.CurrentlyActiveSchedules))
	for _, as := range sa.Status.CurrentlyActiveSchedules {
		scheduleNames = append(scheduleNames, as.ScheduleName)
	}

	act := map[string]any{
		"current":         sa.Status.CurrentProcessingUnits,
		"desired":         builtinDesired,
		"minPU":           minPU,
		"maxPU":           maxPU,
		"now":             now,
		"lastScaleTime":   sa.Status.LastScaleTime.Time,
		"activeSchedules": scheduleNames,
	}

	for _, m := range celMetricSegments {
		if flags&m.flag == 0 {
			continue
		}
		switch m.metric {
		case spannerv1beta1.CPUMetricTypeHighPriority:
			act["cpu."+m.segment] = sa.Status.CurrentHighPriorityCPUUtilization
			act["target."+m.segment] = *sa.Spec.ScaleConfig.TargetCPUUtilization.HighPriority
		case spannerv1beta1.CPUMetricTypeTotal:
			act["cpu."+m.segment] = sa.Status.CurrentTotalCPUUtilization
			act["target."+m.segment] = *sa.Spec.ScaleConfig.TargetCPUUtilization.Total
		}
		for _, w := range windows {
			wm, ok := findWindowMetric(sa.Status.CurrentCPUWindowMetrics, m.metric, w)
			if !ok {
				return nil, fmt.Errorf("%w: metric %s window %s", ErrWindowDataNotReady, m.metric, w)
			}
			for _, agg := range windowAggregations {
				act[fmt.Sprintf("cpu.%s.%s%s", m.segment, agg.name, w)] = agg.value(wm)
			}
		}
	}

	return act, nil
}

func findWindowMetric(metrics []spannerv1beta1.CPUWindowMetric, metric spannerv1beta1.CPUMetricType, window string) (spannerv1beta1.CPUWindowMetric, bool) {
	for _, wm := range metrics {
		if wm.Metric == metric && wm.Window == window {
			return wm, true
		}
	}
	return spannerv1beta1.CPUWindowMetric{}, false
}

func evaluateBool(flags spannerv1beta1.CPUMetricFlags, windows []string, expr string, act map[string]any) (bool, error) {
	prg, err := compiledProgram(flags, windows, expr)
	if err != nil {
		return false, err
	}
	val, _, err := prg.Eval(act)
	if err != nil {
		return false, fmt.Errorf("evaluating CEL expression: %w", err)
	}
	b, ok := val.Value().(bool)
	if !ok {
		return false, fmt.Errorf("CEL expression evaluated to %T, expected bool", val.Value())
	}
	return b, nil
}

// RuleOutcome describes the evaluation of one spec.scaleConfig.scalingRules
// entry during a single reconcile.
type RuleOutcome struct {
	// Index of the rule in spec.scaleConfig.scalingRules.
	Index int
	// When is the rule's CEL condition, echoed for logs and events.
	When string
	// Triggered reports whether the condition evaluated to true.
	Triggered bool
	// CandidatePU is the processing units the rule asks for (current PU plus
	// the rule's scaleUp amount, rounded to a valid PU value and clamped to
	// the effective min/max range). Zero unless Triggered.
	CandidatePU int
	// Err is set when the rule could not be evaluated (compile error on an
	// object that bypassed webhook validation, missing window data, runtime
	// evaluation error). The rule is then skipped — fail-safe toward not
	// triggering an extra scale-up.
	Err error
}

// EvaluateScalingRules evaluates spec.scaleConfig.scalingRules and merges the
// triggered rules' candidates with the built-in logic's desired PU: the
// result is the maximum of builtinDesired and every triggered candidate, so
// rules can only add scaling pressure, never reduce what the built-in logic
// asks for. With no rules configured it returns builtinDesired unchanged.
//
// Rules deliberately bypass scaleupStepSize — the rule's scaleUp is an
// explicit amount, not a request to scale "as needed" — but the hard guards
// still apply: the candidate is clamped to the effective min/max range here,
// and Decide gates the actual application (cooldown intervals, conditions).
func EvaluateScalingRules(sa *spannerv1beta1.SpannerAutoscaler, builtinDesired int, now time.Time) (int, []RuleOutcome) {
	rules := sa.Spec.ScaleConfig.ScalingRules
	if len(rules) == 0 {
		return builtinDesired, nil
	}

	flags := sa.Spec.ScaleConfig.TargetCPUUtilization.ActiveMetricFlags()
	windows := ValidMetricWindows(sa.Spec.ScaleConfig.MetricWindows)
	act, actErr := celActivation(sa, builtinDesired, now)

	desired := builtinDesired
	outcomes := make([]RuleOutcome, 0, len(rules))
	for i, rule := range rules {
		oc := RuleOutcome{Index: i, When: rule.When}
		if actErr != nil {
			oc.Err = actErr
			outcomes = append(outcomes, oc)
			continue
		}
		triggered, err := evaluateBool(flags, windows, rule.When, act)
		if err != nil {
			oc.Err = err
			outcomes = append(outcomes, oc)
			continue
		}
		oc.Triggered = triggered
		if triggered {
			oc.CandidatePU = ruleCandidatePU(sa, rule.ScaleUp)
			desired = max(desired, oc.CandidatePU)
		}
		outcomes = append(outcomes, oc)
	}
	return desired, outcomes
}

// ruleCandidatePU resolves a triggered rule's scaleUp amount against the
// current PU: fixed PU or percent of current, rounded up to a valid PU value
// and clamped to the effective min/max range.
func ruleCandidatePU(sa *spannerv1beta1.SpannerAutoscaler, scaleUp intstr.IntOrString) int {
	current := sa.Status.CurrentProcessingUnits
	amount, err := intstr.GetScaledValueFromIntOrPercent(&scaleUp, current, false)
	if err != nil || amount <= 0 {
		// Invalid amounts cannot occur on a webhook-validated spec; degrade to
		// "no change requested" rather than guessing.
		return current
	}
	candidate := roundUpToValidPU(current + amount)
	minPU, maxPU := effectiveRange(sa)
	return min(max(candidate, minPU), maxPU)
}

// roundUpToValidPU rounds pu up to the nearest valid Spanner processing-unit
// value: a multiple of 100 up to 1000, a multiple of 1000 above.
// https://cloud.google.com/spanner/docs/compute-capacity
func roundUpToValidPU(pu int) int {
	if pu <= 0 {
		return 0
	}
	if pu <= 1000 {
		return ((pu + 99) / 100) * 100
	}
	return ((pu + 999) / 1000) * 1000
}

// effectiveRange returns the autoscaling range in effect: the spec range,
// overridden by the schedule-derived bounds recorded in status (the same
// precedence DesiredPUFromCPU applies).
func effectiveRange(sa *spannerv1beta1.SpannerAutoscaler) (minPU, maxPU int) {
	minPU = sa.Spec.ScaleConfig.ProcessingUnits.Min
	maxPU = sa.Spec.ScaleConfig.ProcessingUnits.Max
	if sa.Status.DesiredMinPUs > 0 {
		minPU = sa.Status.DesiredMinPUs
	}
	if sa.Status.DesiredMaxPUs > 0 {
		maxPU = sa.Status.DesiredMaxPUs
	}
	return minPU, maxPU
}

// Spec field names echoed in GateOutcome.Field.
const (
	GateFieldScaleup   = "scaleupCondition"
	GateFieldScaledown = "scaledownCondition"
)

// GateOutcome describes the evaluation of a scaleupCondition or
// scaledownCondition gate during one Decide call.
type GateOutcome struct {
	// Field is the spec field the gate expression came from:
	// GateFieldScaleup or GateFieldScaledown.
	Field string
	// Allowed reports whether the gate lets the change through. When Err is
	// non-nil it reflects the fail-safe direction instead of an evaluation
	// result: a scale-up gate fails open and a scale-down gate fails closed,
	// so an expression error always errs toward more capacity.
	Allowed bool
	// Err is the evaluation error, if any.
	Err error
}

// evaluateGate evaluates one gate expression. desiredPU is bound to the
// `desired` CEL variable. failOpen selects the fail-safe direction on error.
func evaluateGate(sa *spannerv1beta1.SpannerAutoscaler, field, expr string, desiredPU int, now time.Time, failOpen bool) GateOutcome {
	flags := sa.Spec.ScaleConfig.TargetCPUUtilization.ActiveMetricFlags()
	windows := ValidMetricWindows(sa.Spec.ScaleConfig.MetricWindows)
	act, err := celActivation(sa, desiredPU, now)
	if err != nil {
		return GateOutcome{Field: field, Allowed: failOpen, Err: err}
	}
	allowed, err := evaluateBool(flags, windows, expr, act)
	if err != nil {
		return GateOutcome{Field: field, Allowed: failOpen, Err: err}
	}
	return GateOutcome{Field: field, Allowed: allowed}
}
