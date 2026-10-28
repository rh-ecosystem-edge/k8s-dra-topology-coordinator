package naming

import "testing"

func TestSanitize(t *testing.T) {
	tests := []struct {
		input    string
		fallback string
		want     string
	}{
		{"gpu.nvidia.com", "sub", "gpu-nvidia-com"},
		{"sriovnetwork.k8snetworkplumbingwg.io", "sub", "sriovnetwork-k8snetworkplumbingwg-io"},
		{"dra.cpu", "sub", "dra-cpu"},
		{"simple", "sub", "simple"},
		{"UPPER.Case", "sub", "upper-case"},
		{"UPPER/Case", "sub", "upper-case"},
		{"a..b", "sub", "a-b"},
		{"--a--", "sub", "a"},
		{"", "sub", "sub"},
		{"", "default", "default"},
		{"...", "default", "default"},
		{"-", "x", "x"},
	}
	for _, tt := range tests {
		t.Run(tt.input+"/"+tt.fallback, func(t *testing.T) {
			if got := Sanitize(tt.input, tt.fallback); got != tt.want {
				t.Errorf("Sanitize(%q, %q) = %q, want %q", tt.input, tt.fallback, got, tt.want)
			}
		})
	}
}
