package v1alpha1

import (
	"testing"
)

// TestSchemeGroupVersion 验证 Group / Version 约定
// （Group=edgeflow.io，Version=v1alpha1）。
func TestSchemeGroupVersion(t *testing.T) {
	if SchemeGroupVersion.Group != "edgeflow.io" {
		t.Errorf("Group = %q, 期望 %q", SchemeGroupVersion.Group, "edgeflow.io")
	}
	if SchemeGroupVersion.Version != "v1alpha1" {
		t.Errorf("Version = %q, 期望 %q", SchemeGroupVersion.Version, "v1alpha1")
	}
	if got := SchemeGroupVersion.String(); got != "edgeflow.io/v1alpha1" {
		t.Errorf("String() = %q, 期望 %q", got, "edgeflow.io/v1alpha1")
	}
}
