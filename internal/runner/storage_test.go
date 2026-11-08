package runner

import (
	"testing"

	"gitee.com/kobeyoung81/nanoprotocol"
)

func TestUserName(t *testing.T) {
	cases := []struct {
		display string
		want    string
	}{
		{"Kobe Young", "kobe-young"},
		{"my_agent", "my_agent"},
		{"", "nanosec"},
		{"汉字", "nanosec"},
		{"root", "nanosec"},
		{"ADMIN", "admin"},
	}
	for _, c := range cases {
		got := UserName(nanoprotocol.ContainerRequest{DisplayName: c.display})
		if got != c.want {
			t.Errorf("UserName(%q) = %q, want %q", c.display, got, c.want)
		}
	}
}