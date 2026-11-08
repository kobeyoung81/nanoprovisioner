package netutil

import "testing"

func TestPreferredNetwork(t *testing.T) {
	tests := []struct {
		name  string
		names []string
		want  string
	}{
		{name: "empty", names: nil, want: ""},
		{name: "only defaults", names: []string{"bridge", "host", "none"}, want: ""},
		{name: "first real network", names: []string{"bridge", "devmyriadmind_default", "another"}, want: "devmyriadmind_default"},
		{name: "single real network", names: []string{"myriadmind_default"}, want: "myriadmind_default"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := preferredNetwork(tt.names); got != tt.want {
				t.Errorf("preferredNetwork(%v) = %q, want %q", tt.names, got, tt.want)
			}
		})
	}
}
