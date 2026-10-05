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
	"flag"
	"fmt"
	"strings"
)

// envPrefix starts the name of every environment variable that stands in for a manager flag.
const envPrefix = "SUBNET_OPERATOR_"

// envFlags are the manager flags that can also be set through the environment. OLM passes a
// Subscription's spec.config.env to the manager but no arguments (docs/olm.md), so these are
// the settings an OLM install needs to turn on writes, change events and the other clouds, and
// to point the Azure provider at a sovereign cloud. The rest keep their bundle defaults there.
var envFlags = []string{
	"providers",
	"enable-writes",
	"aws-events-queue-url",
	"aws-events-debounce",
	"gcp-events-subscription",
	"gcp-events-debounce",
	"azure-cloud",
	"azure-authority-host",
	"azure-events-queue-url",
	"azure-events-debounce",
}

// envName is the environment variable for a flag: SUBNET_OPERATOR_ and the flag's name in
// upper case with underscores, e.g. SUBNET_OPERATOR_ENABLE_WRITES for --enable-writes.
func envName(flagName string) string {
	return envPrefix + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// describeEnv adds its environment variable to the help text of each flag of names.
func describeEnv(fs *flag.FlagSet, names []string) {
	for _, name := range names {
		if f := fs.Lookup(name); f != nil {
			f.Usage += fmt.Sprintf(" Also read from %s when the flag is not given.", envName(name))
		}
	}
}

// Where a setting's value came from.
const (
	sourceFlag    = "flag"
	sourceEnv     = "environment"
	sourceDefault = "default"
)

// setting is one of envFlags after applyEnv: its value, where the value came from, and an
// environment variable that was set too but lost to the flag.
type setting struct {
	flag    string
	value   string
	source  string
	env     string
	ignored bool
}

// applyEnv sets every flag of names that was not given on the command line from its
// environment variable, if that is set and not empty. A flag on the command line wins over the
// environment. A value the flag would refuse is an error naming the variable, so that a typo in
// a Subscription stops the operator instead of leaving it read-only without a word.
func applyEnv(fs *flag.FlagSet, names []string, getenv func(string) string) ([]setting, error) {
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	settings := make([]setting, 0, len(names))
	for _, name := range names {
		f := fs.Lookup(name)
		if f == nil {
			return nil, fmt.Errorf("no flag --%s to set from the environment", name)
		}
		s := setting{flag: name, env: envName(name), source: sourceDefault}
		value := getenv(s.env)
		switch {
		case given[name]:
			s.source, s.ignored = sourceFlag, value != ""
		case value != "":
			if err := fs.Set(name, value); err != nil {
				return nil, fmt.Errorf("environment variable %s: invalid value %q for --%s: %w", s.env, value, name, err)
			}
			s.source = sourceEnv
		}
		s.value = f.Value.String()
		settings = append(settings, s)
	}
	return settings, nil
}
