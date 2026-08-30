package runner

import (
	"context"
	"io"
	"os"

	"github.com/hashicorp/terraform-exec/tfexec"
	"github.com/hashicorp/terraform-json"
	tofuexec "github.com/opentofu/tofu-exec/tfexec"
)

// IaCTool abstracts the terraform/tofu CLI execution layer so the controller
// can drive either implementation from a single interface.
type IaCTool interface {
	// Name returns the name of the underlying IaC tool (terraform or opentofu).
	Name() string
	Init(ctx context.Context, opts ...InitOption) error
	Validate(ctx context.Context) (*tfjson.ValidateOutput, error)
	Plan(ctx context.Context, opts ...PlanOption) (bool, error)
	ShowPlanFileRaw(ctx context.Context, planPath string) (string, error)
	Apply(ctx context.Context) error
	Destroy(ctx context.Context) error
	SetEnv(map[string]string) error
	SetStdout(io.Writer)
	SetStderr(io.Writer)
}

// InitOptions holds the neutral init options shared by terraform and tofu.
type InitOptions struct {
	Upgrade bool
}

// InitOption mutates InitOptions.
type InitOption func(*InitOptions)

// WithUpgrade requests `-upgrade` during init.
func WithUpgrade() InitOption {
	return func(o *InitOptions) {
		o.Upgrade = true
	}
}

// PlanOptions holds the neutral plan options shared by terraform and tofu.
type PlanOptions struct {
	Destroy bool
	Out     string
}

// PlanOption mutates PlanOptions.
type PlanOption func(*PlanOptions)

// WithDestroy requests a destroy plan.
func WithDestroy(destroy bool) PlanOption {
	return func(o *PlanOptions) {
		o.Destroy = destroy
	}
}

// WithOut sets the plan output file.
func WithOut(path string) PlanOption {
	return func(o *PlanOptions) {
		o.Out = path
	}
}

// TerraformTool adapts a hashicorp tfexec.Terraform to IaCTool.
type TerraformTool struct {
	tf *tfexec.Terraform
}

var _ IaCTool = (*TerraformTool)(nil)

func NewTerraformTool(tf *tfexec.Terraform) *TerraformTool {
	tf.SetStdout(os.Stdout)
	tf.SetStderr(os.Stderr)
	return &TerraformTool{tf: tf}
}

func (t *TerraformTool) Name() string {
	return "terraform"
}

func (t *TerraformTool) Init(ctx context.Context, opts ...InitOption) error {
	o := InitOptions{}
	for _, opt := range opts {
		opt(&o)
	}
	if o.Upgrade {
		return t.tf.Init(ctx, tfexec.Upgrade(true))
	}
	return t.tf.Init(ctx)
}

func (t *TerraformTool) Validate(ctx context.Context) (*tfjson.ValidateOutput, error) {
	return t.tf.Validate(ctx)
}

func (t *TerraformTool) Plan(ctx context.Context, opts ...PlanOption) (bool, error) {
	o := PlanOptions{}
	for _, opt := range opts {
		opt(&o)
	}
	nativeOpts := []tfexec.PlanOption{tfexec.Destroy(o.Destroy), tfexec.Out(o.Out)}
	return t.tf.Plan(ctx, nativeOpts...)
}

func (t *TerraformTool) ShowPlanFileRaw(ctx context.Context, planPath string) (string, error) {
	return t.tf.ShowPlanFileRaw(ctx, planPath)
}

func (t *TerraformTool) Apply(ctx context.Context) error {
	return t.tf.Apply(ctx)
}

func (t *TerraformTool) Destroy(ctx context.Context) error {
	return t.tf.Destroy(ctx)
}

func (t *TerraformTool) SetEnv(env map[string]string) error {
	return t.tf.SetEnv(env)
}

func (t *TerraformTool) SetStdout(w io.Writer) {
	t.tf.SetStdout(w)
}

func (t *TerraformTool) SetStderr(w io.Writer) {
	t.tf.SetStderr(w)
}

// TofuTool adapts an opentofu tofu-exec Tofu to IaCTool.
type TofuTool struct {
	tf *tofuexec.Tofu
}

var _ IaCTool = (*TofuTool)(nil)

func NewTofuTool(tf *tofuexec.Tofu) *TofuTool {
	tf.SetStdout(os.Stdout)
	tf.SetStderr(os.Stderr)
	return &TofuTool{tf: tf}
}

func (t *TofuTool) Name() string {
	return "opentofu"
}

func (t *TofuTool) Init(ctx context.Context, opts ...InitOption) error {
	o := InitOptions{}
	for _, opt := range opts {
		opt(&o)
	}
	if o.Upgrade {
		return t.tf.Init(ctx, tofuexec.Upgrade(true))
	}
	return t.tf.Init(ctx)
}

func (t *TofuTool) Validate(ctx context.Context) (*tfjson.ValidateOutput, error) {
	return t.tf.Validate(ctx)
}

func (t *TofuTool) Plan(ctx context.Context, opts ...PlanOption) (bool, error) {
	o := PlanOptions{}
	for _, opt := range opts {
		opt(&o)
	}
	nativeOpts := []tofuexec.PlanOption{tofuexec.Destroy(o.Destroy), tofuexec.Out(o.Out)}
	return t.tf.Plan(ctx, nativeOpts...)
}

func (t *TofuTool) ShowPlanFileRaw(ctx context.Context, planPath string) (string, error) {
	return t.tf.ShowPlanFileRaw(ctx, planPath)
}

func (t *TofuTool) Apply(ctx context.Context) error {
	return t.tf.Apply(ctx)
}

func (t *TofuTool) Destroy(ctx context.Context) error {
	return t.tf.Destroy(ctx)
}

func (t *TofuTool) SetEnv(env map[string]string) error {
	return t.tf.SetEnv(env)
}

func (t *TofuTool) SetStdout(w io.Writer) {
	t.tf.SetStdout(w)
}

func (t *TofuTool) SetStderr(w io.Writer) {
	t.tf.SetStderr(w)
}
