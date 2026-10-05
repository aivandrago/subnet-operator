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
	"io"
	"strings"
	"testing"
	"time"
)

// managerFlags defines envFlags on a fresh flag set, with the manager's defaults.
func managerFlags() (*flag.FlagSet, *bool, *string, *time.Duration) {
	fs := flag.NewFlagSet("manager", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	providers := fs.String("providers", "aws", "")
	writes := fs.Bool("enable-writes", false, "")
	fs.String("aws-events-queue-url", "", "")
	debounce := fs.Duration("aws-events-debounce", 10*time.Second, "")
	fs.String("gcp-events-subscription", "", "")
	fs.Duration("gcp-events-debounce", 10*time.Second, "")
	return fs, writes, providers, debounce
}

func TestEnvNames(t *testing.T) {
	want := map[string]string{
		"providers":               "SUBNET_OPERATOR_PROVIDERS",
		"enable-writes":           "SUBNET_OPERATOR_ENABLE_WRITES",
		"aws-events-queue-url":    "SUBNET_OPERATOR_AWS_EVENTS_QUEUE_URL",
		"aws-events-debounce":     "SUBNET_OPERATOR_AWS_EVENTS_DEBOUNCE",
		"gcp-events-subscription": "SUBNET_OPERATOR_GCP_EVENTS_SUBSCRIPTION",
		"gcp-events-debounce":     "SUBNET_OPERATOR_GCP_EVENTS_DEBOUNCE",
	}
	if len(envFlags) != len(want) {
		t.Fatalf("envFlags = %v, want the %d flags documented in docs/olm.md", envFlags, len(want))
	}
	for _, name := range envFlags {
		if got := envName(name); got != want[name] {
			t.Errorf("envName(%q) = %q, want %q", name, got, want[name])
		}
	}
}

// #89: an OLM install sets the manager through the environment; a flag still wins, and the
// source of every value is reported.
func TestApplyEnv(t *testing.T) {
	fs, writes, providers, debounce := managerFlags()
	if err := fs.Parse([]string{"--providers=aws"}); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"SUBNET_OPERATOR_PROVIDERS":            "aws,gcp",
		"SUBNET_OPERATOR_ENABLE_WRITES":        "true",
		"SUBNET_OPERATOR_AWS_EVENTS_QUEUE_URL": "https://sqs.eu-central-1.amazonaws.com/111111111111/q",
		"SUBNET_OPERATOR_AWS_EVENTS_DEBOUNCE":  "5s",
		"SUBNET_OPERATOR_GCP_EVENTS_DEBOUNCE":  "", // empty is unset
	}
	settings, err := applyEnv(fs, envFlags, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if !*writes || *providers != "aws" || *debounce != 5*time.Second {
		t.Errorf("enable-writes=%v providers=%q aws-events-debounce=%v, want true, aws (the flag), 5s",
			*writes, *providers, *debounce)
	}
	got := map[string]setting{}
	for _, s := range settings {
		got[s.flag] = s
	}
	for _, want := range []setting{
		{flag: "providers", value: "aws", source: sourceFlag, env: "SUBNET_OPERATOR_PROVIDERS", ignored: true},
		{flag: "enable-writes", value: "true", source: sourceEnv, env: "SUBNET_OPERATOR_ENABLE_WRITES"},
		{flag: "aws-events-queue-url", value: "https://sqs.eu-central-1.amazonaws.com/111111111111/q",
			source: sourceEnv, env: "SUBNET_OPERATOR_AWS_EVENTS_QUEUE_URL"},
		{flag: "aws-events-debounce", value: "5s", source: sourceEnv, env: "SUBNET_OPERATOR_AWS_EVENTS_DEBOUNCE"},
		{flag: "gcp-events-subscription", value: "", source: sourceDefault, env: "SUBNET_OPERATOR_GCP_EVENTS_SUBSCRIPTION"},
		{flag: "gcp-events-debounce", value: "10s", source: sourceDefault, env: "SUBNET_OPERATOR_GCP_EVENTS_DEBOUNCE"},
	} {
		if got[want.flag] != want {
			t.Errorf("%s: got %+v, want %+v", want.flag, got[want.flag], want)
		}
	}
}

func TestApplyEnvFlagSetToItsDefaultStillWins(t *testing.T) {
	fs, writes, _, _ := managerFlags()
	if err := fs.Parse([]string{"--enable-writes=false"}); err != nil {
		t.Fatal(err)
	}
	settings, err := applyEnv(fs, []string{"enable-writes"},
		func(k string) string { return map[string]string{"SUBNET_OPERATOR_ENABLE_WRITES": "true"}[k] })
	if err != nil {
		t.Fatal(err)
	}
	if *writes || settings[0].source != sourceFlag || !settings[0].ignored {
		t.Errorf("enable-writes=%v %+v, want false from the flag with the variable ignored", *writes, settings[0])
	}
}

func TestApplyEnvRefusesAnInvalidValue(t *testing.T) {
	for env, value := range map[string]string{
		"SUBNET_OPERATOR_ENABLE_WRITES":       "yes please",
		"SUBNET_OPERATOR_AWS_EVENTS_DEBOUNCE": "10",
	} {
		fs, _, _, _ := managerFlags()
		if err := fs.Parse(nil); err != nil {
			t.Fatal(err)
		}
		_, err := applyEnv(fs, envFlags, func(k string) string {
			if k == env {
				return value
			}
			return ""
		})
		if err == nil || !strings.Contains(err.Error(), env) {
			t.Errorf("%s=%q: got %v, want an error naming the variable", env, value, err)
		}
	}
}

func TestApplyEnvNeedsTheFlags(t *testing.T) {
	fs := flag.NewFlagSet("manager", flag.ContinueOnError)
	if _, err := applyEnv(fs, []string{"enable-writes"}, func(string) string { return "" }); err == nil {
		t.Error("got no error for a flag that is not defined")
	}
}
