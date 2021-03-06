package terraform

import (
	"fmt"
	"log"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/terraform/configs/configschema"
	"github.com/hashicorp/terraform/providers"
	"github.com/hashicorp/terraform/tfdiags"
	"github.com/zclconf/go-cty/cty"
)

// NodeApplyableProvider represents a provider during an apply.
type NodeApplyableProvider struct {
	*NodeAbstractProvider
}

var (
	_ GraphNodeExecutable = (*NodeApplyableProvider)(nil)
)

// GraphNodeExecutable
func (n *NodeApplyableProvider) Execute(ctx EvalContext, op walkOperation) (diags tfdiags.Diagnostics) {
	_, err := ctx.InitProvider(n.Addr)
	diags = diags.Append(err)
	if diags.HasErrors() {
		return diags
	}
	provider, _, err := getProvider(ctx, n.Addr)
	diags = diags.Append(err)
	if diags.HasErrors() {
		return diags
	}

	switch op {
	case walkValidate:
		return diags.Append(n.ValidateProvider(ctx, provider))
	case walkPlan, walkApply, walkDestroy:
		return diags.Append(n.ConfigureProvider(ctx, provider, false))
	case walkImport:
		return diags.Append(n.ConfigureProvider(ctx, provider, true))
	}
	return diags
}

func (n *NodeApplyableProvider) ValidateProvider(ctx EvalContext, provider providers.Interface) (diags tfdiags.Diagnostics) {

	configBody := buildProviderConfig(ctx, n.Addr, n.ProviderConfig())

	resp := provider.GetSchema()
	diags = diags.Append(resp.Diagnostics)
	if diags.HasErrors() {
		return diags
	}

	configSchema := resp.Provider.Block
	if configSchema == nil {
		// Should never happen in real code, but often comes up in tests where
		// mock schemas are being used that tend to be incomplete.
		log.Printf("[WARN] ValidateProvider: no config schema is available for %s, so using empty schema", n.Addr)
		configSchema = &configschema.Block{}
	}

	configVal, _, evalDiags := ctx.EvaluateBlock(configBody, configSchema, nil, EvalDataForNoInstanceKey)
	if evalDiags.HasErrors() {
		if n.Config == nil {
			// If there isn't an explicit "provider" block in the configuration,
			// this error message won't be very clear. Add some detail to the
			// error message in this case.
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Invalid provider configuration",
				fmt.Sprintf(providerConfigErr, evalDiags.Err(), n.Addr.Provider),
			))
			return diags
		} else {
			return diags.Append(evalDiags)
		}
	}
	diags = diags.Append(evalDiags)

	// If our config value contains any marked values, ensure those are
	// stripped out before sending this to the provider
	unmarkedConfigVal, _ := configVal.UnmarkDeep()

	req := providers.PrepareProviderConfigRequest{
		Config: unmarkedConfigVal,
	}

	validateResp := provider.PrepareProviderConfig(req)
	diags = diags.Append(validateResp.Diagnostics)

	return diags
}

// ConfigureProvider configures a provider that is already initialized and retrieved.
// If verifyConfigIsKnown is true, ConfigureProvider will return an error if the
// provider configVal is not wholly known and is meant only for use during import.
func (n *NodeApplyableProvider) ConfigureProvider(ctx EvalContext, provider providers.Interface, verifyConfigIsKnown bool) (diags tfdiags.Diagnostics) {
	config := n.ProviderConfig()

	configBody := buildProviderConfig(ctx, n.Addr, config)

	resp := provider.GetSchema()
	diags = diags.Append(resp.Diagnostics)
	if diags.HasErrors() {
		return diags
	}

	configSchema := resp.Provider.Block
	configVal, configBody, evalDiags := ctx.EvaluateBlock(configBody, configSchema, nil, EvalDataForNoInstanceKey)
	diags = diags.Append(evalDiags)
	if evalDiags.HasErrors() {
		return diags
	}

	if verifyConfigIsKnown && !configVal.IsWhollyKnown() {
		diags = diags.Append(&hcl.Diagnostic{
			Severity: hcl.DiagError,
			Summary:  "Invalid provider configuration",
			Detail:   fmt.Sprintf("The configuration for %s depends on values that cannot be determined until apply.", n.Addr),
			Subject:  &config.DeclRange,
		})
		return diags
	}

	// If our config value contains any marked values, ensure those are
	// stripped out before sending this to the provider
	unmarkedConfigVal, _ := configVal.UnmarkDeep()

	// Allow the provider to validate and insert any defaults into the full
	// configuration.
	req := providers.PrepareProviderConfigRequest{
		Config: unmarkedConfigVal,
	}

	// PrepareProviderConfig is only used for validation. We are intentionally
	// ignoring the PreparedConfig field to maintain existing behavior.
	prepareResp := provider.PrepareProviderConfig(req)
	if prepareResp.Diagnostics.HasErrors() {
		if config == nil {
			// If there isn't an explicit "provider" block in the configuration,
			// this error message won't be very clear. Add some detail to the
			// error message in this case.
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Invalid provider configuration",
				fmt.Sprintf(providerConfigErr, prepareResp.Diagnostics.Err(), n.Addr.Provider),
			))
			return diags
		} else {
			return diags.Append(prepareResp.Diagnostics)
		}
	}
	diags = diags.Append(prepareResp.Diagnostics)

	// If the provider returns something different, log a warning to help
	// indicate to provider developers that the value is not used.
	preparedCfg := prepareResp.PreparedConfig
	if preparedCfg != cty.NilVal && !preparedCfg.IsNull() && !preparedCfg.RawEquals(unmarkedConfigVal) {
		log.Printf("[WARN] PrepareProviderConfig from %q changed the config value, but that value is unused", n.Addr)
	}

	configDiags := ctx.ConfigureProvider(n.Addr, unmarkedConfigVal)
	if configDiags.HasErrors() {
		if config == nil {
			// If there isn't an explicit "provider" block in the configuration,
			// this error message won't be very clear. Add some detail to the
			// error message in this case.
			diags = diags.Append(tfdiags.Sourceless(
				tfdiags.Error,
				"Invalid provider configuration",
				fmt.Sprintf(providerConfigErr, configDiags.InConfigBody(configBody).Err(), n.Addr.Provider),
			))
			return diags
		} else {
			return diags.Append(configDiags.InConfigBody(configBody))
		}
	}
	diags = diags.Append(configDiags.InConfigBody(configBody))

	return diags
}

const providerConfigErr = `%s

Provider %q requires explicit configuration. Add a provider block to the root module and configure the provider's required arguments as described in the provider documentation.
`
