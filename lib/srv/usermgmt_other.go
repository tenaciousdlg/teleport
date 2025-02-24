//go:build !linux
// +build !linux

/*
Copyright 2022 Gravitational, Inc.

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

package srv

import (
	"github.com/gravitational/trace"
)

//nolint:staticcheck // intended to always return an error for non-linux builds
func newHostUsersBackend() (HostUsersBackend, error) {
	return nil, trace.NotImplemented("Host user creation management is only supported on linux")
}

//nolint:staticcheck // intended to always return an error for non-linux builds
func newHostSudoersBackend(_ string) (HostSudoersBackend, error) {
	return nil, trace.NotImplemented("Host user creation management is only supported on linux")
}
