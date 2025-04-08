package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"

	"github.com/pkg/errors"
)

var ErrRunning = errors.New("error running command")
var ErrTerraformNotFound = errors.New("terraform not found")

type Terraform struct {
	Stdout        bytes.Buffer
	Stderr        bytes.Buffer
	exec          string
	wd            string
	rootArgs      []string
	commonCmdArgs []string
	env           []string
}

type Version struct {
	TerraformVersion   string   `json:"terraform_version"`
	Platform           string   `json:"platform"`
	ProviderSelections []string `json:"provider_selections"`
	TerraformOutdated  bool     `json:"terraform_outdated"`
}

type Outputs map[string]Output

type Output struct {
	Sensitive bool   `json:"sensitive"`
	Type      string `json:"type"`
	Value     string `json:"value"`
}

func NewTerraform(execPath, wd string) (*Terraform, error) {
	tf := &Terraform{
		exec: execPath,
		wd:   wd,
		rootArgs: []string{
			"-chdir=" + wd,
		},
		commonCmdArgs: []string{
			"-no-color",
			"-json",
		},
		env: []string{
			"TF_IN_AUTOMATION=1",
		},
	}

	cmd := exec.Command(execPath, "version")
	if err := cmd.Run(); err != nil {
		return nil, errors.Wrap(ErrTerraformNotFound, fmt.Sprintf("exec: %s", execPath))
	}

	return tf, nil
}

func (tf *Terraform) runCommand(ctx context.Context, args ...string) ([]byte, error) {
	args = slices.Insert(args, 0, tf.rootArgs...)
	args = append(args, tf.commonCmdArgs...)
	cmd := exec.CommandContext(ctx, "terraform", args...)
	cmd.Stdout = &tf.Stdout
	cmd.Stderr = &tf.Stderr

	err := cmd.Run()

	errStr := tf.Stderr.Bytes()
	outStr := tf.Stdout.Bytes()
	tf.Stdout.Reset()
	tf.Stderr.Reset()

	if err != nil || len(errStr) > 0 {
		return outStr, errors.Wrap(ErrRunning, string(errStr))
	}

	return outStr, nil
}

func (tf *Terraform) Init(ctx context.Context) error {
	_, err := tf.runCommand(ctx, "init", "-upgrade")
	return err
}

func (tf *Terraform) Apply(ctx context.Context) (string, error) {
	res, err := tf.runCommand(ctx, "apply", "-auto-approve")
	return string(res), err
}

func (tf *Terraform) Plan(ctx context.Context) (string, error) {
	res, err := tf.runCommand(ctx, "plan")
	return string(res), err
}

func (tf *Terraform) Output(ctx context.Context) (Outputs, error) {
	output, err := tf.runCommand(ctx, "output")
	if err != nil {
		return Outputs{}, fmt.Errorf("terraform output: %w", err)
	}
	var val Outputs
	if err := json.Unmarshal(output, &val); err != nil {
		return val, fmt.Errorf("unmarshal terraform output: %w", err)
	}

	return val, err
}

func (tf *Terraform) Version(ctx context.Context) (Version, error) {
	res, err := tf.runCommand(ctx, "version")
	if err != nil {
		return Version{}, err
	}
	var v Version
	_ = json.Unmarshal(res, &v)

	return v, nil
}

func (tf *Terraform) Destroy(ctx context.Context) error {
	_, err := tf.runCommand(ctx, "destroy", "-auto-approve")
	return err
}
