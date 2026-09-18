package v1alpha1

import (
	"encoding/json"
	"fmt"
	"maps"

	"github.com/mercari/spanner-autoscaler/api/v1beta1"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/conversion"
)

var log = ctrllog.Log.WithName("spannerautoscaler-v1alpha1.converter")

// preservedFieldsAnnotation carries the v1beta1-only CEL scaling fields
// across a conversion to v1alpha1, which cannot represent them. Without it, a
// client that reads the object as v1alpha1 and writes it back would silently
// strip metricWindows, scalingRules, and both gate conditions from the stored
// v1beta1 object. ConvertFrom stores the fields here; ConvertTo restores them
// and removes the annotation.
const preservedFieldsAnnotation = "spanner.mercari.com/v1beta1-cel-scale-config"

// preservedCELScaleConfig is the annotation payload: the ScaleConfig fields
// that exist only in v1beta1.
type preservedCELScaleConfig struct {
	MetricWindows      []string              `json:"metricWindows,omitempty"`
	ScalingRules       []v1beta1.ScalingRule `json:"scalingRules,omitempty"`
	ScaleupCondition   string                `json:"scaleupCondition,omitempty"`
	ScaledownCondition string                `json:"scaledownCondition,omitempty"`
}

func (p preservedCELScaleConfig) empty() bool {
	return len(p.MetricWindows) == 0 && len(p.ScalingRules) == 0 &&
		p.ScaleupCondition == "" && p.ScaledownCondition == ""
}

func (src *SpannerAutoscaler) ConvertTo(dstRaw conversion.Hub) error {
	log.V(2).Info("begin conversion from v1alpha1 to v1beta1", "src", src)

	dst := dstRaw.(*v1beta1.SpannerAutoscaler)
	dst.Spec.TargetInstance = v1beta1.TargetInstance{
		ProjectID:  *src.Spec.ScaleTargetRef.ProjectID,
		InstanceID: *src.Spec.ScaleTargetRef.InstanceID,
	}

	auth := v1beta1.Authentication{}

	if src.Spec.ImpersonateConfig != nil {
		auth.Type = v1beta1.AuthTypeImpersonation
		auth.ImpersonateConfig = &v1beta1.ImpersonateConfig{
			TargetServiceAccount: src.Spec.ImpersonateConfig.TargetServiceAccount,
			Delegates:            src.Spec.ImpersonateConfig.Delegates,
		}
	}

	if src.Spec.ServiceAccountSecretRef != nil {
		auth.Type = v1beta1.AuthTypeSA
		auth.IAMKeySecret = &v1beta1.IAMKeySecret{
			Name: *src.Spec.ServiceAccountSecretRef.Name,
			Key:  *src.Spec.ServiceAccountSecretRef.Key,
		}

		if src.Spec.ServiceAccountSecretRef.Namespace != nil && *src.Spec.ServiceAccountSecretRef.Namespace != "" {
			auth.IAMKeySecret.Namespace = *src.Spec.ServiceAccountSecretRef.Namespace
		}
	}

	dst.Spec.Authentication = auth

	scaleConfig := v1beta1.ScaleConfig{}
	if src.Spec.MinNodes != nil && *src.Spec.MinNodes >= 1 && src.Spec.MaxNodes != nil && *src.Spec.MaxNodes >= 1 {
		scaleConfig.ComputeType = v1beta1.ComputeTypeNode
		scaleConfig.Nodes = v1beta1.ScaleConfigNodes{
			Min: int(*src.Spec.MinNodes),
			Max: int(*src.Spec.MaxNodes),
		}
	}
	if src.Spec.MinProcessingUnits != nil && *src.Spec.MinProcessingUnits >= 100 && src.Spec.MaxProcessingUnits != nil && *src.Spec.MaxProcessingUnits >= 100 {
		scaleConfig.ComputeType = v1beta1.ComputeTypePU
		scaleConfig.ProcessingUnits = v1beta1.ScaleConfigPUs{
			Min: int(*src.Spec.MinProcessingUnits),
			Max: int(*src.Spec.MaxProcessingUnits),
		}
	}
	if src.Spec.MaxScaleDownNodes != nil {
		scaleConfig.ScaledownStepSize = intstr.FromInt(int(*src.Spec.MaxScaleDownNodes) * 1000)
	}
	hp := int(*src.Spec.TargetCPUUtilization.HighPriority)
	scaleConfig.TargetCPUUtilization = v1beta1.TargetCPUUtilization{
		HighPriority: &hp,
	}

	dst.Spec.ScaleConfig = scaleConfig

	// Copy the resource metadata
	dst.ObjectMeta = src.ObjectMeta

	// Restore the v1beta1-only CEL fields a previous ConvertFrom preserved,
	// then drop the annotation: the hub object carries the real fields.
	if raw, ok := src.Annotations[preservedFieldsAnnotation]; ok {
		var preserved preservedCELScaleConfig
		if err := json.Unmarshal([]byte(raw), &preserved); err != nil {
			return fmt.Errorf("invalid %s annotation: %w", preservedFieldsAnnotation, err)
		}
		dst.Spec.ScaleConfig.MetricWindows = preserved.MetricWindows
		dst.Spec.ScaleConfig.ScalingRules = preserved.ScalingRules
		dst.Spec.ScaleConfig.ScaleupCondition = preserved.ScaleupCondition
		dst.Spec.ScaleConfig.ScaledownCondition = preserved.ScaledownCondition

		dst.Annotations = maps.Clone(dst.Annotations)
		delete(dst.Annotations, preservedFieldsAnnotation)
	}

	// Copy the resource status
	if !src.Status.LastScaleTime.IsZero() {
		dst.Status.LastScaleTime = metav1.Time{Time: src.Status.LastScaleTime.Time}
	}
	if !src.Status.LastSyncTime.IsZero() {
		dst.Status.LastSyncTime = metav1.Time{Time: src.Status.LastSyncTime.Time}
	}
	if src.Status.CurrentProcessingUnits != nil {
		dst.Status.CurrentProcessingUnits = int(*src.Status.CurrentProcessingUnits)
	}
	if src.Status.DesiredProcessingUnits != nil {
		dst.Status.DesiredProcessingUnits = int(*src.Status.DesiredProcessingUnits)
	}
	if src.Status.CurrentHighPriorityCPUUtilization != nil {
		dst.Status.CurrentHighPriorityCPUUtilization = int(*src.Status.CurrentHighPriorityCPUUtilization)
	}
	dst.Status.InstanceState = v1beta1.InstanceState(src.Status.InstanceState)

	log.V(2).Info("finished conversion from v1alpha1 to v1beta1", "src", src, "dst", dst)

	return nil
}

//nolint:gosec
func (dst *SpannerAutoscaler) ConvertFrom(srcRaw conversion.Hub) error {
	src := srcRaw.(*v1beta1.SpannerAutoscaler)
	log.V(2).Info("begin conversion from v1beta1 to v1alpha1", "src", src)

	dst.Spec.ScaleTargetRef = ScaleTargetRef{
		ProjectID:  ptr.To(src.Spec.TargetInstance.ProjectID),
		InstanceID: ptr.To(src.Spec.TargetInstance.InstanceID),
	}

	switch src.Spec.Authentication.Type {
	case v1beta1.AuthTypeSA:
		dst.Spec.ServiceAccountSecretRef = &ServiceAccountSecretRef{
			Name:      ptr.To(src.Spec.Authentication.IAMKeySecret.Name),
			Namespace: ptr.To(src.Spec.Authentication.IAMKeySecret.Namespace),
			Key:       ptr.To(src.Spec.Authentication.IAMKeySecret.Key),
		}
	case v1beta1.AuthTypeImpersonation:
		dst.Spec.ImpersonateConfig = &ImpersonateConfig{
			TargetServiceAccount: src.Spec.Authentication.ImpersonateConfig.TargetServiceAccount,
			Delegates:            src.Spec.Authentication.ImpersonateConfig.Delegates,
		}
	}

	switch src.Spec.ScaleConfig.ComputeType {
	case v1beta1.ComputeTypeNode:
		dst.Spec.MinNodes = ptr.To[int32](int32(src.Spec.ScaleConfig.Nodes.Min))
		dst.Spec.MaxNodes = ptr.To[int32](int32(src.Spec.ScaleConfig.Nodes.Max))

	case v1beta1.ComputeTypePU:
		dst.Spec.MinProcessingUnits = ptr.To[int32](int32(src.Spec.ScaleConfig.ProcessingUnits.Min))
		dst.Spec.MaxProcessingUnits = ptr.To[int32](int32(src.Spec.ScaleConfig.ProcessingUnits.Max))
	}

	if src.Spec.ScaleConfig.ScaledownStepSize.Type == intstr.Int {
		dst.Spec.MaxScaleDownNodes = ptr.To[int32](src.Spec.ScaleConfig.ScaledownStepSize.IntVal / 1000)
	} else {
		dst.Spec.MaxScaleDownNodes = ptr.To[int32](2) // From the default scaledownStepSize value
	}
	if src.Spec.ScaleConfig.TargetCPUUtilization.HighPriority != nil {
		dst.Spec.TargetCPUUtilization = TargetCPUUtilization{
			HighPriority: ptr.To[int32](int32(*src.Spec.ScaleConfig.TargetCPUUtilization.HighPriority)),
		}
	}

	// Copy the resource metadata
	dst.ObjectMeta = src.ObjectMeta

	// v1alpha1 cannot represent the CEL scaling fields; preserve them in an
	// annotation so a v1alpha1 read-modify-write round trip does not strip
	// them from the stored object.
	preserved := preservedCELScaleConfig{
		MetricWindows:      src.Spec.ScaleConfig.MetricWindows,
		ScalingRules:       src.Spec.ScaleConfig.ScalingRules,
		ScaleupCondition:   src.Spec.ScaleConfig.ScaleupCondition,
		ScaledownCondition: src.Spec.ScaleConfig.ScaledownCondition,
	}
	if !preserved.empty() {
		raw, err := json.Marshal(preserved)
		if err != nil {
			return fmt.Errorf("marshaling %s annotation: %w", preservedFieldsAnnotation, err)
		}
		dst.Annotations = maps.Clone(dst.Annotations)
		if dst.Annotations == nil {
			dst.Annotations = map[string]string{}
		}
		dst.Annotations[preservedFieldsAnnotation] = string(raw)
	}

	// Copy the resource status
	dst.Status.LastScaleTime = &metav1.Time{Time: src.Status.LastScaleTime.Time}
	dst.Status.LastSyncTime = &metav1.Time{Time: src.Status.LastSyncTime.Time}
	dst.Status.CurrentNodes = ptr.To[int32](int32(src.Status.CurrentProcessingUnits / 1000))
	dst.Status.CurrentProcessingUnits = ptr.To[int32](int32(src.Status.CurrentProcessingUnits))
	dst.Status.DesiredNodes = ptr.To[int32](int32(src.Status.DesiredProcessingUnits / 1000))
	dst.Status.DesiredProcessingUnits = ptr.To[int32](int32(src.Status.DesiredProcessingUnits))
	dst.Status.CurrentHighPriorityCPUUtilization = ptr.To[int32](int32(src.Status.CurrentHighPriorityCPUUtilization))
	dst.Status.InstanceState = InstanceState(src.Status.InstanceState)
	log.V(2).Info("finished conversion from v1beta1 to v1alpha1", "src", src, "dst", dst)

	return nil
}
