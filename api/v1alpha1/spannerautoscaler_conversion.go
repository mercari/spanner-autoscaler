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

// scaleConfigAnnotation keeps spec.scaleConfig of v1beta1 across a round trip
// through v1alpha1 when it has timeBasedOverrides. v1alpha1 has no such field,
// and dropping them would silently lift the scale-down restrictions they hold.
// Keeping only the overrides is not enough: v1alpha1 also drops base fields
// such as targetCPUUtilization.total that the overrides may refer to, so the
// restored overrides would no longer be valid against the restored base.
const scaleConfigAnnotation = "spanner.mercari.com/v1beta1-scale-config"

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

	scaleConfig := scaleConfigFromV1alpha1(&src.Spec)

	// Copy the resource metadata
	dst.ObjectMeta = src.ObjectMeta

	if raw, ok := src.Annotations[scaleConfigAnnotation]; ok {
		var stored v1beta1.ScaleConfig
		if err := json.Unmarshal([]byte(raw), &stored); err != nil {
			return fmt.Errorf("decoding annotation %s: %w", scaleConfigAnnotation, err)
		}
		scaleConfig = restoreScaleConfig(stored, scaleConfig)
		dst.Annotations = maps.Clone(src.Annotations)
		delete(dst.Annotations, scaleConfigAnnotation)
		if len(dst.Annotations) == 0 {
			dst.Annotations = nil
		}
	}

	dst.Spec.ScaleConfig = scaleConfig

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

	setV1alpha1ScaleFields(&dst.Spec, src.Spec.ScaleConfig)

	// Copy the resource metadata
	dst.ObjectMeta = src.ObjectMeta

	if len(src.Spec.ScaleConfig.TimeBasedOverrides) > 0 {
		raw, err := json.Marshal(src.Spec.ScaleConfig)
		if err != nil {
			return fmt.Errorf("encoding annotation %s: %w", scaleConfigAnnotation, err)
		}
		dst.Annotations = maps.Clone(src.Annotations)
		if dst.Annotations == nil {
			dst.Annotations = map[string]string{}
		}
		dst.Annotations[scaleConfigAnnotation] = string(raw)
	} else if _, ok := src.Annotations[scaleConfigAnnotation]; ok {
		// A stale copy (for example applied through v1beta1 from an exported
		// v1alpha1 manifest) would otherwise be restored over the current
		// scale config by the next v1alpha1 update.
		dst.Annotations = maps.Clone(src.Annotations)
		delete(dst.Annotations, scaleConfigAnnotation)
		if len(dst.Annotations) == 0 {
			dst.Annotations = nil
		}
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

// scaleConfigFromV1alpha1 builds the v1beta1 scale config from the fields that
// v1alpha1 has. Everything else is left at its zero value.
func scaleConfigFromV1alpha1(spec *SpannerAutoscalerSpec) v1beta1.ScaleConfig {
	scaleConfig := v1beta1.ScaleConfig{}
	if spec.MinNodes != nil && *spec.MinNodes >= 1 && spec.MaxNodes != nil && *spec.MaxNodes >= 1 {
		scaleConfig.ComputeType = v1beta1.ComputeTypeNode
		scaleConfig.Nodes = v1beta1.ScaleConfigNodes{
			Min: int(*spec.MinNodes),
			Max: int(*spec.MaxNodes),
		}
	}
	if spec.MinProcessingUnits != nil && *spec.MinProcessingUnits >= 100 && spec.MaxProcessingUnits != nil && *spec.MaxProcessingUnits >= 100 {
		scaleConfig.ComputeType = v1beta1.ComputeTypePU
		scaleConfig.ProcessingUnits = v1beta1.ScaleConfigPUs{
			Min: int(*spec.MinProcessingUnits),
			Max: int(*spec.MaxProcessingUnits),
		}
	}
	if spec.MaxScaleDownNodes != nil {
		scaleConfig.ScaledownStepSize = intstr.FromInt(int(*spec.MaxScaleDownNodes) * 1000)
	}
	if spec.TargetCPUUtilization.HighPriority != nil {
		hp := int(*spec.TargetCPUUtilization.HighPriority)
		scaleConfig.TargetCPUUtilization = v1beta1.TargetCPUUtilization{
			HighPriority: &hp,
		}
	}
	return scaleConfig
}

// setV1alpha1ScaleFields sets the v1alpha1 fields that represent scaleConfig.
//
//nolint:gosec
func setV1alpha1ScaleFields(spec *SpannerAutoscalerSpec, scaleConfig v1beta1.ScaleConfig) {
	switch scaleConfig.ComputeType {
	case v1beta1.ComputeTypeNode:
		spec.MinNodes = ptr.To[int32](int32(scaleConfig.Nodes.Min))
		spec.MaxNodes = ptr.To[int32](int32(scaleConfig.Nodes.Max))

	case v1beta1.ComputeTypePU:
		spec.MinProcessingUnits = ptr.To[int32](int32(scaleConfig.ProcessingUnits.Min))
		spec.MaxProcessingUnits = ptr.To[int32](int32(scaleConfig.ProcessingUnits.Max))
	}

	if scaleConfig.ScaledownStepSize.Type == intstr.Int {
		spec.MaxScaleDownNodes = ptr.To[int32](scaleConfig.ScaledownStepSize.IntVal / 1000)
	} else {
		spec.MaxScaleDownNodes = ptr.To[int32](2) // From the default scaledownStepSize value
	}
	if scaleConfig.TargetCPUUtilization.HighPriority != nil {
		spec.TargetCPUUtilization = TargetCPUUtilization{
			HighPriority: ptr.To[int32](int32(*scaleConfig.TargetCPUUtilization.HighPriority)),
		}
	}
}

// restoreScaleConfig returns the scale config stored in the annotation, with
// the fields a v1alpha1 client changed taken from current. A field counts as
// changed when current differs from what stored becomes after a round trip
// through v1alpha1; otherwise stored keeps the value v1alpha1 cannot hold.
func restoreScaleConfig(stored, current v1beta1.ScaleConfig) v1beta1.ScaleConfig {
	var spec SpannerAutoscalerSpec
	setV1alpha1ScaleFields(&spec, stored)
	unchanged := scaleConfigFromV1alpha1(&spec)

	restored := stored
	if current.ComputeType != unchanged.ComputeType || current.Nodes != unchanged.Nodes || current.ProcessingUnits != unchanged.ProcessingUnits {
		restored.ComputeType = current.ComputeType
		restored.Nodes = current.Nodes
		restored.ProcessingUnits = current.ProcessingUnits
	}
	if current.ScaledownStepSize != unchanged.ScaledownStepSize {
		restored.ScaledownStepSize = current.ScaledownStepSize
	}
	if !ptr.Equal(current.TargetCPUUtilization.HighPriority, unchanged.TargetCPUUtilization.HighPriority) {
		restored.TargetCPUUtilization.HighPriority = current.TargetCPUUtilization.HighPriority
	}
	return restored
}
