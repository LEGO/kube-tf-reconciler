package runner

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
)

var DangerPaths = []string{
	"/",
	".",
	"",
}

type Runner struct {
	tf              *Terraform
	providerConfigs []string
	moduleConfigs   []string

	ApplyResult string
	PlanResult  string
}

func NewRunner(wd string, providerConfigs, moduleConfigs []string) *Runner {
	tf, err := NewTerraform("terraform", wd)
	if err != nil {
		panic(fmt.Sprintf("failed to create terraform: %v", err))
	}

	return &Runner{
		tf:              tf,
		providerConfigs: providerConfigs,
		moduleConfigs:   moduleConfigs,
		ApplyResult:     "",
		PlanResult:      "",
	}
}

func (r *Runner) Init(ctx context.Context) error {
	if slices.Contains(DangerPaths, r.tf.wd) {
		return fmt.Errorf("dangerous path: %s", r.tf.wd)
	}
	r.PlanResult = ""
	r.ApplyResult = ""

	if err := os.RemoveAll(r.tf.wd); err != nil {
		return fmt.Errorf("error removing directory: %w", err)
	}

	if err := os.MkdirAll(r.tf.wd, 0755); err != nil {
		return fmt.Errorf("error creating directory: %w", err)
	}

	sb := strings.Builder{}
	for _, config := range r.providerConfigs {
		sb.WriteString(config)
		sb.WriteString("\n")
	}
	for _, config := range r.moduleConfigs {
		sb.WriteString(config)
		sb.WriteString("\n")
	}

	if err := os.WriteFile(r.tf.wd+"/main.tf", []byte(sb.String()), 0644); err != nil {
		return fmt.Errorf("error writing main.tf: %w", err)
	}

	err := r.tf.Init(ctx)
	if err != nil {
		return fmt.Errorf("error running Init: %w", err)
	}
	return nil
}

func (r *Runner) Execute(ctx context.Context) (Outputs, error) {
	res, err := r.tf.Apply(ctx)
	if err != nil {
		return Outputs{}, err
	}
	r.ApplyResult = res
	outputs, err := r.tf.Output(ctx)
	if err != nil {
		return outputs, fmt.Errorf("error running Output: %w", err)
	}
	return outputs, nil
}
