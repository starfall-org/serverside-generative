package main

import (
	"flag"
	"io"
	"os"
	"testing"
)

func TestWebStartupPasswordPrecedence(t *testing.T) {
	for _, row := range []struct {
		name    string
		args    []string
		env     *string
		want    string
		invalid bool
	}{
		{name: "saved fallback"},
		{name: "environment", env: webTestString("environment-password"), want: "environment-password"},
		{name: "flag only", args: []string{"--web-password", "command-line-password"}, want: "command-line-password"},
		{name: "flag precedence", args: []string{"--web-password=command-line-password"}, env: webTestString("environment-password"), want: "command-line-password"},
		{name: "explicit empty flag", args: []string{"--web-password="}, env: webTestString("environment-password"), invalid: true},
		{name: "explicit empty environment", env: webTestString(""), invalid: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Setenv("GENERATIVE_WEB_PASSWORD", "")
			if row.env == nil {
				_ = os.Unsetenv("GENERATIVE_WEB_PASSWORD")
			} else {
				t.Setenv("GENERATIVE_WEB_PASSWORD", *row.env)
			}
			flags := flag.NewFlagSet("serve", flag.ContinueOnError)
			flags.SetOutput(io.Discard)
			password := flags.String("web-password", "", "")
			if err := flags.Parse(row.args); err != nil {
				t.Fatal(err)
			}
			got, err := resolveWebStartupPassword(flags, password)
			switch {
			case row.invalid:
				if err == nil {
					t.Fatal("empty input accepted")
				}
			case row.want == "":
				if got != nil || err != nil {
					t.Fatal("did not defer to saved password")
				}
			default:
				if got == nil || *got != row.want || err != nil {
					t.Fatal("wrong startup override source")
				}
			}
		})
	}
}

func webTestString(value string) *string { return &value }
