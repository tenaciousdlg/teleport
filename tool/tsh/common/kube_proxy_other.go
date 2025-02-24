//go:build !darwin && !linux
// +build !darwin,!linux

/*
Copyright 2023 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package common

import (
	"context"
	"runtime"

	"github.com/gravitational/trace"
)

func reexecToShell(_ context.Context, _ []byte) error {
	return trace.NotImplemented("headless mode for local Kubernetes proxy is not implemented for %s", runtime.GOOS)
}
