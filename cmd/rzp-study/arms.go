package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Arms: more than one generator, run against the SAME frozen task set.
//
// The study originally assumed one run. That assumption is baked into three
// places -- a single study/model.frozen.json, a single study/traces, and a
// validateTraceSet that compares every trace against the CURRENT manifest -- and
// it breaks the moment a second generator is added, because pre-registering the
// second arm edits PROTOCOL.md, which changes freeze_sha256, which retroactively
// invalidates the 45 traces of the first.
//
// That would be exactly backwards. Arm A ran under the freeze that was current
// when it ran; a later amendment cannot make it not have. So each arm records
// the freeze and model freeze it actually ran under, here, and is validated
// against ITS OWN record rather than against whatever the protocol says today.
//
// arms.json is deliberately NOT part of the freeze it describes. Including a
// file that records a freeze hash inside the thing being hashed is circular.

const armsFile = "arms.json"

type arm struct {
	Arm    string `json:"arm"`
	Model  string `json:"model_freeze"` // relative to study/
	Traces string `json:"traces"`
	Sheet  string `json:"worksheet"`
	Labels string `json:"labels"`
	Report string `json:"results"`

	// What this arm ACTUALLY ran under. Empty until the arm has run; filled by
	// `arms record`, which reads the traces rather than being told.
	FreezeSHA      string `json:"freeze_sha256,omitempty"`
	ModelFreezeSHA string `json:"model_freeze_sha256,omitempty"`
	ServedModel    string `json:"served_model,omitempty"`
	Status         string `json:"status"` // declared | complete

	Note string `json:"note,omitempty"`
}

type armsRegistry struct {
	// Note survives a round trip so the reason this file sits outside the
	// freeze stays attached to the file rather than only to this source.
	Note string `json:"note,omitempty"`
	Arms []arm  `json:"arms"`
}

func loadArms() (*armsRegistry, error) {
	b, err := os.ReadFile(filepath.Join(studyDir(), armsFile))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", armsFile, err)
	}
	var r armsRegistry
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (r *armsRegistry) find(name string) (*arm, error) {
	for i := range r.Arms {
		if strings.EqualFold(r.Arms[i].Arm, name) {
			return &r.Arms[i], nil
		}
	}
	var names []string
	for _, a := range r.Arms {
		names = append(names, a.Arm)
	}
	sort.Strings(names)
	return nil, fmt.Errorf("no arm %q in study/%s; declared arms: %s",
		name, armsFile, strings.Join(names, ", "))
}

// path resolves an arm-relative path against study/.
func (a *arm) path(rel string) string { return filepath.Join(studyDir(), rel) }

func (a *arm) modelPath() string  { return a.path(a.Model) }
func (a *arm) tracePath() string  { return a.path(a.Traces) }
func (a *arm) sheetPath() string  { return a.path(a.Sheet) }
func (a *arm) labelPath() string  { return a.path(a.Labels) }
func (a *arm) reportPath() string { return a.path(a.Report) }

func (r *armsRegistry) save() error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(studyDir(), armsFile), append(b, '\n'), 0o644)
}

// cmdArms lists the declared arms, or records what one actually ran under.
func cmdArms(args []string) error {
	r, err := loadArms()
	if err != nil {
		return err
	}

	if len(args) >= 2 && args[0] == "record" {
		a, err := r.find(args[1])
		if err != nil {
			return err
		}
		traces, err := loadTraces(a.tracePath())
		if err != nil {
			return err
		}
		if len(traces) == 0 {
			return fmt.Errorf("arm %s has no traces at %s", a.Arm, a.tracePath())
		}
		// Read the facts off the traces. Being told them would defeat the point.
		fr, mf, sm := map[string]bool{}, map[string]bool{}, map[string]bool{}
		for _, t := range traces {
			fr[t.FreezeSHA] = true
			mf[t.ModelFreezeSHA] = true
			if t.ServedModel != "" {
				sm[t.ServedModel] = true
			}
		}
		if len(fr) != 1 || len(mf) != 1 {
			return fmt.Errorf("arm %s traces are not internally consistent: "+
				"%d distinct freezes, %d distinct model freezes", a.Arm, len(fr), len(mf))
		}
		if len(sm) > 1 {
			return fmt.Errorf("arm %s traces report %d distinct served models", a.Arm, len(sm))
		}
		for k := range fr {
			a.FreezeSHA = k
		}
		for k := range mf {
			a.ModelFreezeSHA = k
		}
		for k := range sm {
			a.ServedModel = k
		}
		a.Status = "complete"
		if err := r.save(); err != nil {
			return err
		}
		fmt.Printf("arm %s recorded: %d traces, freeze %.12s, model freeze %.12s, served %s\n",
			a.Arm, len(traces), a.FreezeSHA, a.ModelFreezeSHA, a.ServedModel)
		return nil
	}

	fmt.Printf("%-4s %-10s %-24s %-16s %s\n", "ARM", "STATUS", "SERVED MODEL", "FREEZE", "TRACES")
	for _, a := range r.Arms {
		served, fr := a.ServedModel, a.FreezeSHA
		if served == "" {
			served = "-"
		}
		if fr == "" {
			fr = "-"
		} else if len(fr) > 12 {
			fr = fr[:12]
		}
		n := "-"
		if entries, err := filepath.Glob(filepath.Join(a.tracePath(), "*.json")); err == nil && len(entries) > 0 {
			n = fmt.Sprint(len(entries))
		}
		fmt.Printf("%-4s %-10s %-24s %-16s %s\n", a.Arm, a.Status, served, fr, n)
	}
	return nil
}
