package main

import (
	"testing"

	"go.viam.com/test"
)

func TestParseRunArgs(t *testing.T) {
	tests := []struct {
		name          string
		args          []string
		wantTo        string
		wantMatch     string
		wantCmdArgs   []string
		wantHelp      bool
		wantErrSubstr string
	}{
		{
			name:        "plain command with flags",
			args:        []string{"tail", "-f", "hi"},
			wantCmdArgs: []string{"tail", "-f", "hi"},
		},
		{
			name:        "short to flag",
			args:        []string{"-t", "myconn", "tail", "-f", "hi"},
			wantTo:      "myconn",
			wantCmdArgs: []string{"tail", "-f", "hi"},
		},
		{
			name:        "long to flag",
			args:        []string{"--to", "myconn", "tail", "-f", "hi"},
			wantTo:      "myconn",
			wantCmdArgs: []string{"tail", "-f", "hi"},
		},
		{
			name:        "long to flag with equals",
			args:        []string{"--to=myconn", "tail", "-f", "hi"},
			wantTo:      "myconn",
			wantCmdArgs: []string{"tail", "-f", "hi"},
		},
		{
			name:        "double dash separator",
			args:        []string{"--", "tail", "-f", "hi"},
			wantCmdArgs: []string{"tail", "-f", "hi"},
		},
		{
			name:        "to flag then double dash",
			args:        []string{"-t", "myconn", "--", "tail", "-f"},
			wantTo:      "myconn",
			wantCmdArgs: []string{"tail", "-f"},
		},
		{
			name:     "help long flag",
			args:     []string{"--help"},
			wantHelp: true,
		},
		{
			name:     "help short flag",
			args:     []string{"-h"},
			wantHelp: true,
		},
		{
			name:          "no args",
			args:          []string{},
			wantErrSubstr: "command required",
		},
		{
			name:          "missing to value",
			args:          []string{"-t"},
			wantErrSubstr: "requires a value",
		},
		{
			name:        "short match flag",
			args:        []string{"-m", "prod-*", "uptime"},
			wantMatch:   "prod-*",
			wantCmdArgs: []string{"uptime"},
		},
		{
			name:        "long match flag",
			args:        []string{"--match", "web-?", "df", "-h"},
			wantMatch:   "web-?",
			wantCmdArgs: []string{"df", "-h"},
		},
		{
			name:        "match flag with equals",
			args:        []string{"--match=*", "hostname"},
			wantMatch:   "*",
			wantCmdArgs: []string{"hostname"},
		},
		{
			name:          "missing match value",
			args:          []string{"-m"},
			wantErrSubstr: "requires a value",
		},
		{
			name:        "match flag then double dash",
			args:        []string{"-m", "prod-*", "--", "tail", "-f"},
			wantMatch:   "prod-*",
			wantCmdArgs: []string{"tail", "-f"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ra, helpRequested, err := parseRunArgs(tt.args)
			if tt.wantErrSubstr != "" {
				test.That(t, err, test.ShouldNotBeNil)
				test.That(t, err.Error(), test.ShouldContainSubstring, tt.wantErrSubstr)

				return
			}

			test.That(t, err, test.ShouldBeNil)
			test.That(t, helpRequested, test.ShouldEqual, tt.wantHelp)

			if tt.wantHelp {
				return
			}

			test.That(t, ra.to, test.ShouldEqual, tt.wantTo)
			test.That(t, ra.match, test.ShouldEqual, tt.wantMatch)
			test.That(t, ra.command, test.ShouldResemble, tt.wantCmdArgs)
		})
	}
}
