package main

import "testing"

func TestIsPublishCommand(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		arguments []string
		want      bool
	}{
		{name: "publish", arguments: []string{"publish"}, want: true},
		{name: "case and whitespace", arguments: []string{" Publish "}, want: true},
		{name: "missing", arguments: nil, want: false},
		{name: "unknown", arguments: []string{"print"}, want: false},
		{name: "extra", arguments: []string{"publish", "contract"}, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := isPublishCommand(test.arguments); got != test.want {
				t.Fatalf("isPublishCommand(%v) = %t, want %t", test.arguments, got, test.want)
			}
		})
	}
}
