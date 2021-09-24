// Copyright (c) 2021 Doc.ai and/or its affiliates.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at:
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package vrf

import (
	"sync"
)

type vrfInfo struct {
	/* vrf ID */
	id uint32

	/* count - the number of clients using this vrf ID */
	count uint32
}

type Map struct {
	/* entries - is a map[NetworkServiceName]{vrfId, count} */
	entries map[string]*vrfInfo

	/* mutex for entries */
	mut sync.Mutex
}

func NewMap() *Map {
	return &Map{
		entries: make(map[string]*vrfInfo),
	}
}

type options struct {
	v4 *Map
	v6 *Map
}

// Option is an option pattern for upClient/Server
type Option func(o *options)

// WithSharedMapV4 - sets shared vrfV4 map. It may be needed for sharing vrf between client and server
func WithSharedMapV4(v *Map) Option {
	return func(o *options) {
		o.v4 = v
	}
}

// WithSharedMapV6 - sets shared vrfV6 map. It may be needed for sharing vrf between client and server
func WithSharedMapV6(v *Map) Option {
	return func(o *options) {
		o.v6 = v
	}
}