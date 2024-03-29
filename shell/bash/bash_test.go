// Copyright 2019 Drone.IO Inc. All rights reserved.
// Use of this source code is governed by the Polyform License
// that can be found in the LICENSE file.

package bash

import (
	"reflect"
	"testing"
)

func TestCommands(t *testing.T) {
	cmd, args := Command()
	{
		got, want := cmd, "/bin/sh"
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Want command %v, got %v", want, got)
		}
	}
	{
		got, want := args, []string{"-e"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Want command %v, got %v", want, got)
		}
	}
}

func TestScript(t *testing.T) {
	got, want := Script([]string{"go build", "go test"}), exampleScript
	if got != want {
		t.Errorf("Want %q, got %q", want, got)
	}
}

func TestSilentScript(t *testing.T) {
	got, want := SilentScript([]string{"go build", "go test"}), exampleSilentScript
	if got != want {
		t.Errorf("Want %q, got %q", want, got)
	}
}

var exampleScript = `
set -e

echo + "go build"
go build

echo + "go test"
go test
`

var exampleSilentScript = `
set -e

go build

go test
`
