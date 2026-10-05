/*
Copyright 2026.

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

package main

import (
	"errors"
	"flag"
	"fmt"
	"slices"
	"strings"
)

// removedSetting is a flag or an environment variable that an earlier release deprecated and
// this one removed, with what replaces it.
type removedSetting struct {
	name        string
	replacement string
}

// removedFlagNames are the manager flags deprecated in 0.9 and removed in 1.0.
var removedFlagNames = []removedSetting{
	{"events-queue-url", "--aws-events-queue-url"},
	{"events-debounce", "--aws-events-debounce"},
}

// removedEnv are the environment variables deprecated in 0.9 and removed in 1.0.
var removedEnv = []removedSetting{
	{"EVENTS_QUEUE_URL", "--aws-events-queue-url"},
}

// removedFlags remembers which removed flags were passed. They stay defined so that passing one
// is refused with its replacement, rather than with the flag package's "flag provided but not
// defined", and so that an environment variable that is still set does not go unnoticed: the
// operator would run without its event queue and nobody would be told.
type removedFlags struct {
	used []string
}

type removedFlag struct {
	setting removedSetting
	into    *removedFlags
}

func (f *removedFlag) String() string { return "" }

func (f *removedFlag) Set(string) error {
	f.into.used = append(f.into.used, f.setting.name)
	return nil
}

// registerRemovedFlags defines the removed flags on fs.
func registerRemovedFlags(fs *flag.FlagSet) *removedFlags {
	r := &removedFlags{}
	for _, s := range removedFlagNames {
		fs.Var(&removedFlag{setting: s, into: r}, s.name,
			fmt.Sprintf("Removed in 1.0: use %s. The operator refuses to start with it.", s.replacement))
	}
	return r
}

// check returns an error naming every removed flag that was passed and every removed
// environment variable that is set, with its replacement, or nil when there are none.
func (r *removedFlags) check(getenv func(string) string) error {
	var problems []string
	for _, s := range removedFlagNames {
		if slices.Contains(r.used, s.name) {
			problems = append(problems, fmt.Sprintf("--%s was removed in 1.0; use %s", s.name, s.replacement))
		}
	}
	for _, s := range removedEnv {
		if getenv(s.name) != "" {
			problems = append(problems, fmt.Sprintf("the environment variable %s was removed in 1.0; use %s",
				s.name, s.replacement))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return errors.New(strings.Join(problems, "; "))
}
