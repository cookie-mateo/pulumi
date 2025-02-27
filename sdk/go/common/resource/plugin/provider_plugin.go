// Copyright 2016-2018, Pulumi Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/blang/semver"
	pbempty "github.com/golang/protobuf/ptypes/empty"
	_struct "github.com/golang/protobuf/ptypes/struct"
	"github.com/grpc-ecosystem/grpc-opentracing/go/otgrpc"
	multierror "github.com/hashicorp/go-multierror"
	"github.com/opentracing/opentracing-go"
	"github.com/pkg/errors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/cmdutil"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/contract"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/logging"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/rpcutil"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/rpcutil/rpcerror"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
)

// The `Type()` for the NodeJS dynamic provider.  Logically, this is the same as calling
// providers.MakeProviderType(tokens.Package("pulumi-nodejs")), but does not depend on the providers package
// (a direct dependency would cause a cyclic import issue.
//
// This is needed because we have to handle some buggy behavior that previous versions of this provider implemented.
const nodejsDynamicProviderType = "pulumi:providers:pulumi-nodejs"

// The `Type()` for the Kubernetes provider.  Logically, this is the same as calling
// providers.MakeProviderType(tokens.Package("kubernetes")), but does not depend on the providers package
// (a direct dependency would cause a cyclic import issue.
//
// This is needed because we have to handle some buggy behavior that previous versions of this provider implemented.
const kubernetesProviderType = "pulumi:providers:kubernetes"

// provider reflects a resource plugin, loaded dynamically for a single package.
type provider struct {
	ctx                    *Context                         // a plugin context for caching, etc.
	pkg                    tokens.Package                   // the Pulumi package containing this provider's resources.
	plug                   *plugin                          // the actual plugin process wrapper.
	clientRaw              pulumirpc.ResourceProviderClient // the raw provider client; usually unsafe to use directly.
	disableProviderPreview bool                             // true if previews for Create and Update are disabled.
	legacyPreview          bool                             // enables legacy behavior for unconfigured provider previews.

	// Await and provide configuration values.
	// Use of closures here prevents unintentional unguarded access
	// to the configuration values.
	awaitConfig   func(context.Context) (pluginConfig, error)
	provideConfig func(pluginConfig, error)
}

// pluginConfig holds the configuration of the provider
// as specified by the Configure call.
type pluginConfig struct {
	known bool // true if all configuration values are known.

	acceptSecrets   bool // true if this plugin accepts strongly-typed secrets.
	acceptResources bool // true if this plugin accepts strongly-typed resource refs.
	acceptOutputs   bool // true if this plugin accepts output values.
	supportsPreview bool // true if this plugin supports previews for Create and Update.
}

// pluginConfigPromise is an asynchronously filled value for pluginConfig.
type pluginConfigPromise struct {
	done chan struct{} // closed on set
	once sync.Once     // only one set
	cfg  pluginConfig
	err  error // non-nil if the operation failed
}

func newPluginConfigPromise() *pluginConfigPromise {
	return &pluginConfigPromise{
		done: make(chan struct{}),
	}
}

// Await blocks until the value in the promise has resolved
// or the given context expires.
func (p *pluginConfigPromise) Await(ctx context.Context) (pluginConfig, error) {
	select {
	case <-ctx.Done():
		return pluginConfig{}, ctx.Err()
	case <-p.done:
		return p.cfg, p.err
	}
}

// Fulfill provides values to the promise.
// A non-nil error indicates failure.
//
// Fulfill can be called only once.
// Any calls after that will be ignored.
func (p *pluginConfigPromise) Fulfill(cfg pluginConfig, err error) {
	p.once.Do(func() {
		defer close(p.done)
		p.cfg = cfg
		p.err = err
	})
}

// NewProvider attempts to bind to a given package's resource plugin and then creates a gRPC connection to it.  If the
// plugin could not be found, or an error occurs while creating the child process, an error is returned.
func NewProvider(host Host, ctx *Context, pkg tokens.Package, version *semver.Version,
	options map[string]interface{}, disableProviderPreview bool, jsonConfig string,
) (Provider, error) {
	// See if this is a provider we just want to attach to
	var plug *plugin
	var optAttach string
	if providersEnvVar, has := os.LookupEnv("PULUMI_DEBUG_PROVIDERS"); has {
		for _, provider := range strings.Split(providersEnvVar, ",") {
			parts := strings.SplitN(provider, ":", 2)

			if parts[0] == pkg.String() {
				optAttach = parts[1]
				break
			}
		}
	}

	prefix := fmt.Sprintf("%v (resource)", pkg)

	if optAttach != "" {
		port, err := strconv.Atoi(optAttach)
		if err != nil {
			return nil, fmt.Errorf("Expected a numeric port, got %s in PULUMI_DEBUG_PROVIDERS: %w",
				optAttach, err)
		}

		conn, err := dialPlugin(port, pkg.String(), prefix, providerPluginDialOptions(ctx, pkg, ""))
		if err != nil {
			return nil, err
		}

		// Done; store the connection and return the plugin info.
		plug = &plugin{
			Conn: conn,
			// Nothing to kill
			Kill: func() error { return nil },
		}
	} else {
		// Load the plugin's path by using the standard workspace logic.
		path, err := workspace.GetPluginPath(
			workspace.ResourcePlugin, strings.ReplaceAll(string(pkg), tokens.QNameDelimiter, "_"),
			version, host.GetProjectPlugins())
		if err != nil {
			return nil, err
		}

		contract.Assertf(path != "", "unexpected empty path for plugin %s", pkg)

		// Runtime options are passed as environment variables to the provider, this is _currently_ used by
		// dynamic providers to do things like lookup the virtual environment to use.
		env := os.Environ()
		for k, v := range options {
			env = append(env, fmt.Sprintf("PULUMI_RUNTIME_%s=%v", strings.ToUpper(k), v))
		}
		if jsonConfig != "" {
			env = append(env, fmt.Sprintf("PULUMI_CONFIG=%s", jsonConfig))
		}
		plug, err = newPlugin(ctx, ctx.Pwd, path, prefix,
			workspace.ResourcePlugin, []string{host.ServerAddr()}, env, providerPluginDialOptions(ctx, pkg, ""))
		if err != nil {
			return nil, err
		}
	}

	contract.Assertf(plug != nil, "unexpected nil resource plugin for %s", pkg)

	legacyPreview := cmdutil.IsTruthy(os.Getenv("PULUMI_LEGACY_PROVIDER_PREVIEW"))

	cfgPromise := newPluginConfigPromise()
	p := &provider{
		ctx:                    ctx,
		pkg:                    pkg,
		plug:                   plug,
		clientRaw:              pulumirpc.NewResourceProviderClient(plug.Conn),
		disableProviderPreview: disableProviderPreview,
		legacyPreview:          legacyPreview,
		awaitConfig:            cfgPromise.Await,
		provideConfig:          cfgPromise.Fulfill,
	}

	// If we just attached (i.e. plugin bin is nil) we need to call attach
	if plug.Bin == "" {
		err := p.Attach(host.ServerAddr())
		if err != nil {
			return nil, err
		}
	}

	return p, nil
}

func providerPluginDialOptions(ctx *Context, pkg tokens.Package, path string) []grpc.DialOption {
	dialOpts := append(
		rpcutil.OpenTracingInterceptorDialOptions(otgrpc.SpanDecorator(decorateProviderSpans)),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		rpcutil.GrpcChannelOptions(),
	)

	if ctx.DialOptions != nil {
		metadata := map[string]interface{}{
			"mode": "client",
			"kind": "resource",
		}
		if pkg != "" {
			metadata["name"] = pkg.String()
		}
		if path != "" {
			metadata["path"] = path
		}
		dialOpts = append(dialOpts, ctx.DialOptions(metadata)...)
	}

	return dialOpts
}

// NewProviderFromPath creates a new provider by loading the plugin binary located at `path`.
func NewProviderFromPath(host Host, ctx *Context, path string) (Provider, error) {
	env := os.Environ()

	plug, err := newPlugin(ctx, ctx.Pwd, path, "",
		workspace.ResourcePlugin, []string{host.ServerAddr()}, env, providerPluginDialOptions(ctx, "", path))
	if err != nil {
		return nil, err
	}
	contract.Assertf(plug != nil, "unexpected nil resource plugin at %q", path)

	legacyPreview := cmdutil.IsTruthy(os.Getenv("PULUMI_LEGACY_PROVIDER_PREVIEW"))

	cfgPromise := newPluginConfigPromise()
	p := &provider{
		ctx:           ctx,
		plug:          plug,
		clientRaw:     pulumirpc.NewResourceProviderClient(plug.Conn),
		legacyPreview: legacyPreview,
		awaitConfig:   cfgPromise.Await,
		provideConfig: cfgPromise.Fulfill,
	}

	// If we just attached (i.e. plugin bin is nil) we need to call attach
	if plug.Bin == "" {
		err := p.Attach(host.ServerAddr())
		if err != nil {
			return nil, err
		}
	}
	return p, nil
}

func NewProviderWithClient(ctx *Context, pkg tokens.Package, client pulumirpc.ResourceProviderClient,
	disableProviderPreview bool,
) Provider {
	cfgPromise := newPluginConfigPromise()
	return &provider{
		ctx:                    ctx,
		pkg:                    pkg,
		clientRaw:              client,
		disableProviderPreview: disableProviderPreview,
		awaitConfig:            cfgPromise.Await,
		provideConfig:          cfgPromise.Fulfill,
	}
}

func (p *provider) Pkg() tokens.Package { return p.pkg }

// label returns a base label for tracing functions.
func (p *provider) label() string {
	return fmt.Sprintf("Provider[%s, %p]", p.pkg, p)
}

func (p *provider) requestContext() context.Context {
	if p.ctx == nil {
		return context.Background()
	}
	return p.ctx.Request()
}

// isDiffCheckConfigLogicallyUnimplemented returns true when an rpcerror.Error should be treated as if it was an error
// due to a rpc being unimplemented. Due to past mistakes, different providers returned "Unimplemented" in a variaity of
// different ways that don't always result in an Uimplemented error code.
func isDiffCheckConfigLogicallyUnimplemented(err *rpcerror.Error, providerType tokens.Type) bool {
	switch string(providerType) {
	// The NodeJS dynamic provider implementation incorrectly returned an empty message instead of properly implementing
	// Diff/CheckConfig.  This gets turned into a error with type: "Internal".
	case nodejsDynamicProviderType:
		if err.Code() == codes.Internal {
			logging.V(8).Infof("treating error %s as unimplemented error", err)
			return true
		}

	// The Kubernetes provider returned an "Unimplmeneted" message, but it did so by returning a status from a different
	// package that the provider was expected. That caused the error to be wrapped with an "Unknown" error.
	case kubernetesProviderType:
		if err.Code() == codes.Unknown && strings.Contains(err.Message(), "Unimplemented") {
			logging.V(8).Infof("treating error %s as unimplemented error", err)
			return true
		}
	}

	return false
}

// GetSchema fetches the schema for this resource provider, if any.
func (p *provider) GetSchema(version int) ([]byte, error) {
	resp, err := p.clientRaw.GetSchema(p.requestContext(), &pulumirpc.GetSchemaRequest{
		Version: int32(version),
	})
	if err != nil {
		return nil, err
	}
	return []byte(resp.GetSchema()), nil
}

// CheckConfig validates the configuration for this resource provider.
func (p *provider) CheckConfig(urn resource.URN, olds,
	news resource.PropertyMap, allowUnknowns bool,
) (resource.PropertyMap, []CheckFailure, error) {
	label := fmt.Sprintf("%s.CheckConfig(%s)", p.label(), urn)
	logging.V(7).Infof("%s executing (#olds=%d,#news=%d)", label, len(olds), len(news))

	molds, err := MarshalProperties(olds, MarshalOptions{
		Label:        fmt.Sprintf("%s.olds", label),
		KeepUnknowns: allowUnknowns,
	})
	if err != nil {
		return nil, nil, err
	}

	mnews, err := MarshalProperties(news, MarshalOptions{
		Label:        fmt.Sprintf("%s.news", label),
		KeepUnknowns: allowUnknowns,
	})
	if err != nil {
		return nil, nil, err
	}

	resp, err := p.clientRaw.CheckConfig(p.requestContext(), &pulumirpc.CheckRequest{
		Urn:  string(urn),
		Olds: molds,
		News: mnews,
	})
	if err != nil {
		rpcError := rpcerror.Convert(err)
		code := rpcError.Code()
		if code == codes.Unimplemented || isDiffCheckConfigLogicallyUnimplemented(rpcError, urn.Type()) {
			// For backwards compatibility, just return the news as if the provider was okay with them.
			logging.V(7).Infof("%s unimplemented rpc: returning news as is", label)
			return news, nil, nil
		}
		logging.V(8).Infof("%s provider received rpc error `%s`: `%s`", label, rpcError.Code(),
			rpcError.Message())
		return nil, nil, err
	}

	// Unmarshal the provider inputs.
	var inputs resource.PropertyMap
	if ins := resp.GetInputs(); ins != nil {
		inputs, err = UnmarshalProperties(ins, MarshalOptions{
			Label:          fmt.Sprintf("%s.inputs", label),
			KeepUnknowns:   allowUnknowns,
			RejectUnknowns: !allowUnknowns,
			KeepSecrets:    true,
			KeepResources:  true,
		})
		if err != nil {
			return nil, nil, err
		}
	}

	// And now any properties that failed verification.
	failures := make([]CheckFailure, 0, len(resp.GetFailures()))
	for _, failure := range resp.GetFailures() {
		failures = append(failures, CheckFailure{resource.PropertyKey(failure.Property), failure.Reason})
	}

	// Copy over any secret annotations, since we could not pass any to the provider, and return.
	annotateSecrets(inputs, news)
	logging.V(7).Infof("%s success: inputs=#%d failures=#%d", label, len(inputs), len(failures))
	return inputs, failures, nil
}

func decodeDetailedDiff(resp *pulumirpc.DiffResponse) map[string]PropertyDiff {
	if !resp.GetHasDetailedDiff() {
		return nil
	}

	detailedDiff := make(map[string]PropertyDiff)
	for k, v := range resp.GetDetailedDiff() {
		var d DiffKind
		switch v.GetKind() {
		case pulumirpc.PropertyDiff_ADD:
			d = DiffAdd
		case pulumirpc.PropertyDiff_ADD_REPLACE:
			d = DiffAddReplace
		case pulumirpc.PropertyDiff_DELETE:
			d = DiffDelete
		case pulumirpc.PropertyDiff_DELETE_REPLACE:
			d = DiffDeleteReplace
		case pulumirpc.PropertyDiff_UPDATE:
			d = DiffUpdate
		case pulumirpc.PropertyDiff_UPDATE_REPLACE:
			d = DiffUpdateReplace
		default:
			// Consider unknown diff kinds to be simple updates.
			d = DiffUpdate
		}
		detailedDiff[k] = PropertyDiff{
			Kind:      d,
			InputDiff: v.GetInputDiff(),
		}
	}

	return detailedDiff
}

// DiffConfig checks what impacts a hypothetical change to this provider's configuration will have on the provider.
func (p *provider) DiffConfig(urn resource.URN, olds, news resource.PropertyMap,
	allowUnknowns bool, ignoreChanges []string,
) (DiffResult, error) {
	label := fmt.Sprintf("%s.DiffConfig(%s)", p.label(), urn)
	logging.V(7).Infof("%s executing (#olds=%d,#news=%d)", label, len(olds), len(news))
	molds, err := MarshalProperties(olds, MarshalOptions{
		Label:        fmt.Sprintf("%s.olds", label),
		KeepUnknowns: true,
	})
	if err != nil {
		return DiffResult{}, err
	}

	mnews, err := MarshalProperties(news, MarshalOptions{
		Label:        fmt.Sprintf("%s.news", label),
		KeepUnknowns: true,
	})
	if err != nil {
		return DiffResult{}, err
	}

	resp, err := p.clientRaw.DiffConfig(p.requestContext(), &pulumirpc.DiffRequest{
		Urn:           string(urn),
		Olds:          molds,
		News:          mnews,
		IgnoreChanges: ignoreChanges,
	})
	if err != nil {
		rpcError := rpcerror.Convert(err)
		code := rpcError.Code()
		if code == codes.Unimplemented || isDiffCheckConfigLogicallyUnimplemented(rpcError, urn.Type()) {
			logging.V(7).Infof("%s unimplemented rpc: returning DiffUnknown with no replaces", label)
			// In this case, the provider plugin did not implement this and we have to provide some answer:
			//
			// There are two interesting scenarios with the present gRPC interface:
			// 1. Configuration differences in which all properties are known
			// 2. Configuration differences in which some new property is unknown.
			//
			// In both cases, we return a diff result that indicates that the provider _should not_ be replaced.
			// Although this decision is not conservative--indeed, the conservative decision would be to always require
			// replacement of a provider if any input has changed--we believe that it results in the best possible user
			// experience for providers that do not implement DiffConfig functionality. If we took the conservative
			// route here, any change to a provider's configuration (no matter how inconsequential) would cause all of
			// its resources to be replaced. This is clearly a bad experience, and differs from how things worked prior
			// to first-class providers.
			return DiffResult{Changes: DiffUnknown, ReplaceKeys: nil}, nil
		}
		logging.V(8).Infof("%s provider received rpc error `%s`: `%s`", label, rpcError.Code(),
			rpcError.Message())
		return DiffResult{}, nil
	}

	replaces := make([]resource.PropertyKey, 0, len(resp.GetReplaces()))
	for _, replace := range resp.GetReplaces() {
		replaces = append(replaces, resource.PropertyKey(replace))
	}
	stables := make([]resource.PropertyKey, 0, len(resp.GetStables()))
	for _, stable := range resp.GetStables() {
		stables = append(stables, resource.PropertyKey(stable))
	}
	diffs := make([]resource.PropertyKey, 0, len(resp.GetDiffs()))
	for _, diff := range resp.GetDiffs() {
		diffs = append(diffs, resource.PropertyKey(diff))
	}

	changes := resp.GetChanges()
	deleteBeforeReplace := resp.GetDeleteBeforeReplace()
	logging.V(7).Infof("%s success: changes=%d #replaces=%v #stables=%v delbefrepl=%v, diffs=#%v",
		label, changes, replaces, stables, deleteBeforeReplace, diffs)

	return DiffResult{
		Changes:             DiffChanges(changes),
		ReplaceKeys:         replaces,
		StableKeys:          stables,
		ChangedKeys:         diffs,
		DetailedDiff:        decodeDetailedDiff(resp),
		DeleteBeforeReplace: deleteBeforeReplace,
	}, nil
}

// annotateSecrets copies the "secretness" from the ins to the outs. If there are values with the same keys for the
// outs and the ins, if they are both objects, they are transformed recursively. Otherwise, if the value in the ins
// contains a secret, the entire out value is marked as a secret.  This is very close to how we project secrets
// in the programming model, with one small difference, which is how we treat the case where both are objects. In the
// programming model, we would say the entire output object is a secret. Here, we actually recur in. We do this because
// we don't want a single secret value in a rich structure to taint the entire object. Doing so would mean things like
// the entire value in the deployment would be encrypted instead of a small chunk. It also means the entire property
// would be displayed as `[secret]` in the CLI instead of a small part.
//
// NOTE: This means that for an array, if any value in the input version is a secret, the entire output array is
// marked as a secret. This is actually a very nice result, because often arrays are treated like sets by providers
// and the order may not be preserved across an operation. This means we do end up encrypting the entire array
// but that's better than accidentally leaking a value which just moved to a different location.
func annotateSecrets(outs, ins resource.PropertyMap) {
	if outs == nil || ins == nil {
		return
	}

	for key, inValue := range ins {
		outValue, has := outs[key]
		if !has {
			continue
		}
		if outValue.IsObject() && inValue.IsObject() {
			annotateSecrets(outValue.ObjectValue(), inValue.ObjectValue())
		} else if !outValue.IsSecret() && inValue.ContainsSecrets() {
			outs[key] = resource.MakeSecret(outValue)
		}
	}
}

func removeSecrets(v resource.PropertyValue) interface{} {
	switch {
	case v.IsNull():
		return nil
	case v.IsBool():
		return v.BoolValue()
	case v.IsNumber():
		return v.NumberValue()
	case v.IsString():
		return v.StringValue()
	case v.IsArray():
		arr := []interface{}{}
		for _, v := range v.ArrayValue() {
			arr = append(arr, removeSecrets(v))
		}
		return arr
	case v.IsAsset():
		return v.AssetValue()
	case v.IsArchive():
		return v.ArchiveValue()
	case v.IsComputed():
		return v.Input()
	case v.IsOutput():
		return v.OutputValue()
	case v.IsSecret():
		return removeSecrets(v.SecretValue().Element)
	default:
		contract.Assertf(v.IsObject(), "v is not Object '%v' instead", v.TypeString())
		obj := map[string]interface{}{}
		for k, v := range v.ObjectValue() {
			obj[string(k)] = removeSecrets(v)
		}
		return obj
	}
}

// Configure configures the resource provider with "globals" that control its behavior.
func (p *provider) Configure(inputs resource.PropertyMap) error {
	label := fmt.Sprintf("%s.Configure()", p.label())
	logging.V(7).Infof("%s executing (#vars=%d)", label, len(inputs))

	// Convert the inputs to a config map. If any are unknown, do not configure the underlying plugin: instead, leave
	// the cfgknown bit unset and carry on.
	config := make(map[string]string)
	for k, v := range inputs {
		if k == "version" {
			continue
		}

		if v.ContainsUnknowns() {
			p.provideConfig(pluginConfig{
				known:           false,
				acceptSecrets:   false,
				acceptResources: false,
			}, nil)
			return nil
		}

		mapped := removeSecrets(v)
		if _, isString := mapped.(string); !isString {
			marshalled, err := json.Marshal(mapped)
			if err != nil {
				err := errors.Wrapf(err, "marshaling configuration property '%v'", k)
				p.provideConfig(pluginConfig{}, err)
				return err
			}
			mapped = string(marshalled)
		}

		// Pass the older spelling of a configuration key across the RPC interface, for now, to support
		// providers which are on the older plan.
		config[string(p.Pkg())+":config:"+string(k)] = mapped.(string)
	}

	minputs, err := MarshalProperties(inputs, MarshalOptions{
		Label:         fmt.Sprintf("%s.inputs", label),
		KeepUnknowns:  true,
		KeepSecrets:   true,
		KeepResources: true,
	})
	if err != nil {
		err := errors.Wrapf(err, "marshaling provider inputs")
		p.provideConfig(pluginConfig{}, err)
		return err
	}

	// Spawn the configure to happen in parallel.  This ensures that we remain responsive elsewhere that might
	// want to make forward progress, even as the configure call is happening.
	go func() {
		resp, err := p.clientRaw.Configure(p.requestContext(), &pulumirpc.ConfigureRequest{
			AcceptSecrets:   true,
			AcceptResources: true,
			Variables:       config,
			Args:            minputs,
		})
		if err != nil {
			rpcError := rpcerror.Convert(err)
			logging.V(7).Infof("%s failed: err=%v", label, rpcError.Message())
			err = createConfigureError(rpcError)
		}

		p.provideConfig(pluginConfig{
			known:           true,
			acceptSecrets:   resp.GetAcceptSecrets(),
			acceptResources: resp.GetAcceptResources(),
			supportsPreview: resp.GetSupportsPreview(),
			acceptOutputs:   resp.GetAcceptOutputs(),
		}, err)
	}()

	return nil
}

// Check validates that the given property bag is valid for a resource of the given type.
func (p *provider) Check(urn resource.URN,
	olds, news resource.PropertyMap,
	allowUnknowns bool, randomSeed []byte,
) (resource.PropertyMap, []CheckFailure, error) {
	label := fmt.Sprintf("%s.Check(%s)", p.label(), urn)
	logging.V(7).Infof("%s executing (#olds=%d,#news=%d)", label, len(olds), len(news))

	// Ensure that the plugin is configured.
	client := p.clientRaw
	pcfg, err := p.awaitConfig(context.Background())
	if err != nil {
		return nil, nil, err
	}

	// If the configuration for this provider was not fully known--e.g. if we are doing a preview and some input
	// property was sourced from another resource's output properties--don't call into the underlying provider.
	if !pcfg.known {
		return news, nil, nil
	}

	molds, err := MarshalProperties(olds, MarshalOptions{
		Label:         fmt.Sprintf("%s.olds", label),
		KeepUnknowns:  allowUnknowns,
		KeepSecrets:   pcfg.acceptSecrets,
		KeepResources: pcfg.acceptResources,
	})
	if err != nil {
		return nil, nil, err
	}
	mnews, err := MarshalProperties(news, MarshalOptions{
		Label:         fmt.Sprintf("%s.news", label),
		KeepUnknowns:  allowUnknowns,
		KeepSecrets:   pcfg.acceptSecrets,
		KeepResources: pcfg.acceptResources,
	})
	if err != nil {
		return nil, nil, err
	}

	resp, err := client.Check(p.requestContext(), &pulumirpc.CheckRequest{
		Urn:        string(urn),
		Olds:       molds,
		News:       mnews,
		RandomSeed: randomSeed,
	})
	if err != nil {
		rpcError := rpcerror.Convert(err)
		logging.V(7).Infof("%s failed: err=%v", label, rpcError.Message())
		return nil, nil, rpcError
	}

	// Unmarshal the provider inputs.
	var inputs resource.PropertyMap
	if ins := resp.GetInputs(); ins != nil {
		inputs, err = UnmarshalProperties(ins, MarshalOptions{
			Label:          fmt.Sprintf("%s.inputs", label),
			KeepUnknowns:   allowUnknowns,
			RejectUnknowns: !allowUnknowns,
			KeepSecrets:    true,
			KeepResources:  true,
		})
		if err != nil {
			return nil, nil, err
		}
	}

	// If we could not pass secrets to the provider, retain the secret bit on any property with the same name. This
	// allows us to retain metadata about secrets in many cases, even for providers that do not understand secrets
	// natively.
	if !pcfg.acceptSecrets {
		annotateSecrets(inputs, news)
	}

	// And now any properties that failed verification.
	failures := make([]CheckFailure, 0, len(resp.GetFailures()))
	for _, failure := range resp.GetFailures() {
		failures = append(failures, CheckFailure{resource.PropertyKey(failure.Property), failure.Reason})
	}

	logging.V(7).Infof("%s success: inputs=#%d failures=#%d", label, len(inputs), len(failures))
	return inputs, failures, nil
}

// Diff checks what impacts a hypothetical update will have on the resource's properties.
func (p *provider) Diff(urn resource.URN, id resource.ID,
	olds resource.PropertyMap, news resource.PropertyMap, allowUnknowns bool,
	ignoreChanges []string,
) (DiffResult, error) {
	contract.Assertf(urn != "", "Diff requires a URN")
	contract.Assertf(id != "", "Diff requires an ID")
	contract.Assertf(news != nil, "Diff requires new properties")
	contract.Assertf(olds != nil, "Diff requires old properties")

	label := fmt.Sprintf("%s.Diff(%s,%s)", p.label(), urn, id)
	logging.V(7).Infof("%s: executing (#olds=%d,#news=%d)", label, len(olds), len(news))

	// Ensure that the plugin is configured.
	client := p.clientRaw
	pcfg, err := p.awaitConfig(context.Background())
	if err != nil {
		return DiffResult{}, err
	}

	// If the configuration for this provider was not fully known--e.g. if we are doing a preview and some input
	// property was sourced from another resource's output properties--don't call into the underlying provider.
	// Instead, indicate that the diff is unavailable and write a message
	if !pcfg.known {
		logging.V(7).Infof("%s: cannot diff due to unknown config", label)
		const message = "The provider for this resource has inputs that are not known during preview.\n" +
			"This preview may not correctly represent the changes that will be applied during an update."
		return DiffResult{}, DiffUnavailable(message)
	}

	molds, err := MarshalProperties(olds, MarshalOptions{
		Label:              fmt.Sprintf("%s.olds", label),
		ElideAssetContents: true,
		KeepUnknowns:       allowUnknowns,
		KeepSecrets:        pcfg.acceptSecrets,
		KeepResources:      pcfg.acceptResources,
	})
	if err != nil {
		return DiffResult{}, err
	}
	mnews, err := MarshalProperties(news, MarshalOptions{
		Label:         fmt.Sprintf("%s.news", label),
		KeepUnknowns:  allowUnknowns,
		KeepSecrets:   pcfg.acceptSecrets,
		KeepResources: pcfg.acceptResources,
	})
	if err != nil {
		return DiffResult{}, err
	}

	resp, err := client.Diff(p.requestContext(), &pulumirpc.DiffRequest{
		Id:            string(id),
		Urn:           string(urn),
		Olds:          molds,
		News:          mnews,
		IgnoreChanges: ignoreChanges,
	})
	if err != nil {
		rpcError := rpcerror.Convert(err)
		logging.V(7).Infof("%s failed: %v", label, rpcError.Message())
		return DiffResult{}, rpcError
	}

	replaces := make([]resource.PropertyKey, 0, len(resp.GetReplaces()))
	for _, replace := range resp.GetReplaces() {
		replaces = append(replaces, resource.PropertyKey(replace))
	}
	stables := make([]resource.PropertyKey, 0, len(resp.GetStables()))
	for _, stable := range resp.GetStables() {
		stables = append(stables, resource.PropertyKey(stable))
	}
	diffs := make([]resource.PropertyKey, 0, len(resp.GetDiffs()))
	for _, diff := range resp.GetDiffs() {
		diffs = append(diffs, resource.PropertyKey(diff))
	}

	changes := resp.GetChanges()
	deleteBeforeReplace := resp.GetDeleteBeforeReplace()
	logging.V(7).Infof("%s success: changes=%d #replaces=%v #stables=%v delbefrepl=%v, diffs=#%v, detaileddiff=%v",
		label, changes, replaces, stables, deleteBeforeReplace, diffs, resp.GetDetailedDiff())

	return DiffResult{
		Changes:             DiffChanges(changes),
		ReplaceKeys:         replaces,
		StableKeys:          stables,
		ChangedKeys:         diffs,
		DetailedDiff:        decodeDetailedDiff(resp),
		DeleteBeforeReplace: deleteBeforeReplace,
	}, nil
}

// Create allocates a new instance of the provided resource and assigns its unique resource.ID and outputs afterwards.
func (p *provider) Create(urn resource.URN, props resource.PropertyMap, timeout float64, preview bool) (resource.ID,
	resource.PropertyMap, resource.Status, error,
) {
	contract.Assertf(urn != "", "Create requires a URN")
	contract.Assertf(props != nil, "Create requires properties")

	label := fmt.Sprintf("%s.Create(%s)", p.label(), urn)
	logging.V(7).Infof("%s executing (#props=%v)", label, len(props))

	// Ensure that the plugin is configured.
	client := p.clientRaw
	pcfg, err := p.awaitConfig(context.Background())
	if err != nil {
		return "", nil, resource.StatusOK, err
	}

	// If this is a preview and the plugin does not support provider previews, or if the configuration for the provider
	// is not fully known, hand back an empty property map. This will force the language SDK will to treat all properties
	// as unknown, which is conservatively correct.
	//
	// If the provider does not support previews, return the inputs as the state. Note that this can cause problems for
	// the language SDKs if there are input and state properties that share a name but expect differently-shaped values.
	if preview {
		// TODO: it would be great to swap the order of these if statements. This would prevent a behavioral change for
		// providers that do not support provider previews, which will always return the inputs as state regardless of
		// whether or not the config is known. Unfortunately, we can't, since the `supportsPreview` bit depends on the
		// result of `Configure`, which we won't call if the `cfgknown` is false. It may be worth fixing this catch-22
		// by extending the provider gRPC interface with a `SupportsFeature` API similar to the language monitor.
		if !pcfg.known {
			if p.legacyPreview {
				return "", props, resource.StatusOK, nil
			}
			return "", resource.PropertyMap{}, resource.StatusOK, nil
		}
		if !pcfg.supportsPreview || p.disableProviderPreview {
			return "", props, resource.StatusOK, nil
		}
	}

	// We should only be calling {Create,Update,Delete} if the provider is fully configured.
	contract.Assertf(pcfg.known, "Create cannot be called if the configuration is unknown")

	mprops, err := MarshalProperties(props, MarshalOptions{
		Label:         fmt.Sprintf("%s.inputs", label),
		KeepUnknowns:  preview,
		KeepSecrets:   pcfg.acceptSecrets,
		KeepResources: pcfg.acceptResources,
	})
	if err != nil {
		return "", nil, resource.StatusOK, err
	}

	var id resource.ID
	var liveObject *_struct.Struct
	var resourceError error
	resourceStatus := resource.StatusOK
	resp, err := client.Create(p.requestContext(), &pulumirpc.CreateRequest{
		Urn:        string(urn),
		Properties: mprops,
		Timeout:    timeout,
		Preview:    preview,
	})
	if err != nil {
		resourceStatus, id, liveObject, _, resourceError = parseError(err)
		logging.V(7).Infof("%s failed: %v", label, resourceError)

		if resourceStatus != resource.StatusPartialFailure {
			return "", nil, resourceStatus, resourceError
		}
		// Else it's a `StatusPartialFailure`.
	} else {
		id = resource.ID(resp.GetId())
		liveObject = resp.GetProperties()
	}

	if id == "" && !preview {
		return "", nil, resource.StatusUnknown,
			errors.Errorf("plugin for package '%v' returned empty resource.ID from create '%v'", p.pkg, urn)
	}

	outs, err := UnmarshalProperties(liveObject, MarshalOptions{
		Label:          fmt.Sprintf("%s.outputs", label),
		RejectUnknowns: !preview,
		KeepUnknowns:   preview,
		KeepSecrets:    true,
		KeepResources:  true,
	})
	if err != nil {
		return "", nil, resourceStatus, err
	}

	// If we could not pass secrets to the provider, retain the secret bit on any property with the same name. This
	// allows us to retain metadata about secrets in many cases, even for providers that do not understand secrets
	// natively.
	if !pcfg.acceptSecrets {
		annotateSecrets(outs, props)
	}

	logging.V(7).Infof("%s success: id=%s; #outs=%d", label, id, len(outs))
	if resourceError == nil {
		return id, outs, resourceStatus, nil
	}
	return id, outs, resourceStatus, resourceError
}

// read the current live state associated with a resource.  enough state must be include in the inputs to uniquely
// identify the resource; this is typically just the resource id, but may also include some properties.
func (p *provider) Read(urn resource.URN, id resource.ID,
	inputs, state resource.PropertyMap,
) (ReadResult, resource.Status, error) {
	contract.Assertf(urn != "", "Read URN was empty")
	contract.Assertf(id != "", "Read ID was empty")

	label := fmt.Sprintf("%s.Read(%s,%s)", p.label(), id, urn)
	logging.V(7).Infof("%s executing (#inputs=%v, #state=%v)", label, len(inputs), len(state))

	// Ensure that the plugin is configured.
	client := p.clientRaw
	pcfg, err := p.awaitConfig(context.Background())
	if err != nil {
		return ReadResult{}, resource.StatusUnknown, err
	}

	// If the provider is not fully configured, return an empty bag.
	if !pcfg.known {
		return ReadResult{
			Outputs: resource.PropertyMap{},
			Inputs:  resource.PropertyMap{},
		}, resource.StatusUnknown, nil
	}

	// Marshal the resource inputs and state so we can perform the RPC.
	var minputs *_struct.Struct
	if inputs != nil {
		m, err := MarshalProperties(inputs, MarshalOptions{
			Label:              label,
			ElideAssetContents: true,
			KeepSecrets:        pcfg.acceptSecrets,
			KeepResources:      pcfg.acceptResources,
		})
		if err != nil {
			return ReadResult{}, resource.StatusUnknown, err
		}
		minputs = m
	}
	mstate, err := MarshalProperties(state, MarshalOptions{
		Label:              label,
		ElideAssetContents: true,
		KeepSecrets:        pcfg.acceptSecrets,
		KeepResources:      pcfg.acceptResources,
	})
	if err != nil {
		return ReadResult{}, resource.StatusUnknown, err
	}

	// Now issue the read request over RPC, blocking until it finished.
	var readID resource.ID
	var liveObject *_struct.Struct
	var liveInputs *_struct.Struct
	var resourceError error
	resourceStatus := resource.StatusOK
	resp, err := client.Read(p.requestContext(), &pulumirpc.ReadRequest{
		Id:         string(id),
		Urn:        string(urn),
		Properties: mstate,
		Inputs:     minputs,
	})
	if err != nil {
		resourceStatus, readID, liveObject, liveInputs, resourceError = parseError(err)
		logging.V(7).Infof("%s failed: %v", label, err)

		if resourceStatus != resource.StatusPartialFailure {
			return ReadResult{}, resourceStatus, resourceError
		}
		// Else it's a `StatusPartialFailure`.
	} else {
		readID = resource.ID(resp.GetId())
		liveObject = resp.GetProperties()
		liveInputs = resp.GetInputs()
	}

	// If the resource was missing, simply return a nil property map.
	if string(readID) == "" {
		return ReadResult{}, resourceStatus, nil
	}

	// Finally, unmarshal the resulting state properties and return them.
	newState, err := UnmarshalProperties(liveObject, MarshalOptions{
		Label:          fmt.Sprintf("%s.outputs", label),
		RejectUnknowns: true,
		KeepSecrets:    true,
		KeepResources:  true,
	})
	if err != nil {
		return ReadResult{}, resourceStatus, err
	}

	var newInputs resource.PropertyMap
	if liveInputs != nil {
		newInputs, err = UnmarshalProperties(liveInputs, MarshalOptions{
			Label:          label + ".inputs",
			RejectUnknowns: true,
			KeepSecrets:    true,
			KeepResources:  true,
		})
		if err != nil {
			return ReadResult{}, resourceStatus, err
		}
	}

	// If we could not pass secrets to the provider, retain the secret bit on any property with the same name. This
	// allows us to retain metadata about secrets in many cases, even for providers that do not understand secrets
	// natively.
	if !pcfg.acceptSecrets {
		annotateSecrets(newInputs, inputs)
		annotateSecrets(newState, state)
	}

	logging.V(7).Infof("%s success; #outs=%d, #inputs=%d", label, len(newState), len(newInputs))
	return ReadResult{
		ID:      readID,
		Outputs: newState,
		Inputs:  newInputs,
	}, resourceStatus, resourceError
}

// Update updates an existing resource with new values.
func (p *provider) Update(urn resource.URN, id resource.ID,
	olds resource.PropertyMap, news resource.PropertyMap, timeout float64,
	ignoreChanges []string, preview bool,
) (resource.PropertyMap, resource.Status, error) {
	contract.Assertf(urn != "", "Update requires a URN")
	contract.Assertf(id != "", "Update requires an ID")
	contract.Assertf(news != nil, "Update requires new properties")
	contract.Assertf(olds != nil, "Update requires old properties")

	label := fmt.Sprintf("%s.Update(%s,%s)", p.label(), id, urn)
	logging.V(7).Infof("%s executing (#olds=%v,#news=%v)", label, len(olds), len(news))

	// Ensure that the plugin is configured.
	client := p.clientRaw
	pcfg, err := p.awaitConfig(context.Background())
	if err != nil {
		return news, resource.StatusOK, err
	}

	// If this is a preview and the plugin does not support provider previews, or if the configuration for the provider
	// is not fully known, hand back an empty property map. This will force the language SDK to treat all properties
	// as unknown, which is conservatively correct.
	//
	// If the provider does not support previews, return the inputs as the state. Note that this can cause problems for
	// the language SDKs if there are input and state properties that share a name but expect differently-shaped values.
	if preview {
		// TODO: it would be great to swap the order of these if statements. This would prevent a behavioral change for
		// providers that do not support provider previews, which will always return the inputs as state regardless of
		// whether or not the config is known. Unfortunately, we can't, since the `supportsPreview` bit depends on the
		// result of `Configure`, which we won't call if the `cfgknown` is false. It may be worth fixing this catch-22
		// by extending the provider gRPC interface with a `SupportsFeature` API similar to the language monitor.
		if !pcfg.known {
			if p.legacyPreview {
				return news, resource.StatusOK, nil
			}
			return resource.PropertyMap{}, resource.StatusOK, nil
		}
		if !pcfg.supportsPreview || p.disableProviderPreview {
			return news, resource.StatusOK, nil
		}
	}

	// We should only be calling {Create,Update,Delete} if the provider is fully configured.
	contract.Assertf(pcfg.known, "Update cannot be called if the configuration is unknown")

	molds, err := MarshalProperties(olds, MarshalOptions{
		Label:              fmt.Sprintf("%s.olds", label),
		ElideAssetContents: true,
		KeepSecrets:        pcfg.acceptSecrets,
		KeepResources:      pcfg.acceptResources,
	})
	if err != nil {
		return nil, resource.StatusOK, err
	}
	mnews, err := MarshalProperties(news, MarshalOptions{
		Label:         fmt.Sprintf("%s.news", label),
		KeepUnknowns:  preview,
		KeepSecrets:   pcfg.acceptSecrets,
		KeepResources: pcfg.acceptResources,
	})
	if err != nil {
		return nil, resource.StatusOK, err
	}

	var liveObject *_struct.Struct
	var resourceError error
	resourceStatus := resource.StatusOK
	resp, err := client.Update(p.requestContext(), &pulumirpc.UpdateRequest{
		Id:            string(id),
		Urn:           string(urn),
		Olds:          molds,
		News:          mnews,
		Timeout:       timeout,
		IgnoreChanges: ignoreChanges,
		Preview:       preview,
	})
	if err != nil {
		resourceStatus, _, liveObject, _, resourceError = parseError(err)
		logging.V(7).Infof("%s failed: %v", label, resourceError)

		if resourceStatus != resource.StatusPartialFailure {
			return nil, resourceStatus, resourceError
		}
		// Else it's a `StatusPartialFailure`.
	} else {
		liveObject = resp.GetProperties()
	}

	outs, err := UnmarshalProperties(liveObject, MarshalOptions{
		Label:          fmt.Sprintf("%s.outputs", label),
		RejectUnknowns: !preview,
		KeepUnknowns:   preview,
		KeepSecrets:    true,
		KeepResources:  true,
	})
	if err != nil {
		return nil, resourceStatus, err
	}

	// If we could not pass secrets to the provider, retain the secret bit on any property with the same name. This
	// allows us to retain metadata about secrets in many cases, even for providers that do not understand secrets
	// natively.
	if !pcfg.acceptSecrets {
		annotateSecrets(outs, news)
	}

	logging.V(7).Infof("%s success; #outs=%d", label, len(outs))
	if resourceError == nil {
		return outs, resourceStatus, nil
	}
	return outs, resourceStatus, resourceError
}

// Delete tears down an existing resource.
func (p *provider) Delete(urn resource.URN, id resource.ID, props resource.PropertyMap,
	timeout float64,
) (resource.Status, error) {
	contract.Assertf(urn != "", "Delete requires a URN")
	contract.Assertf(id != "", "Delete requires an ID")

	label := fmt.Sprintf("%s.Delete(%s,%s)", p.label(), urn, id)
	logging.V(7).Infof("%s executing (#props=%d)", label, len(props))

	// Ensure that the plugin is configured.
	client := p.clientRaw
	pcfg, err := p.awaitConfig(context.Background())
	if err != nil {
		return resource.StatusOK, err
	}

	mprops, err := MarshalProperties(props, MarshalOptions{
		Label:              label,
		ElideAssetContents: true,
		KeepSecrets:        pcfg.acceptSecrets,
		KeepResources:      pcfg.acceptResources,
	})
	if err != nil {
		return resource.StatusOK, err
	}

	// We should only be calling {Create,Update,Delete} if the provider is fully configured.
	contract.Assertf(pcfg.known, "Delete cannot be called if the configuration is unknown")

	if _, err := client.Delete(p.requestContext(), &pulumirpc.DeleteRequest{
		Id:         string(id),
		Urn:        string(urn),
		Properties: mprops,
		Timeout:    timeout,
	}); err != nil {
		resourceStatus, rpcErr := resourceStateAndError(err)
		logging.V(7).Infof("%s failed: %v", label, rpcErr)
		return resourceStatus, rpcErr
	}

	logging.V(7).Infof("%s success", label)
	return resource.StatusOK, nil
}

// Construct creates a new component resource from the given type, name, parent, options, and inputs, and returns
// its URN and outputs.
func (p *provider) Construct(info ConstructInfo, typ tokens.Type, name tokens.QName, parent resource.URN,
	inputs resource.PropertyMap, options ConstructOptions,
) (ConstructResult, error) {
	contract.Assertf(typ != "", "Construct requires a type")
	contract.Assertf(name != "", "Construct requires a name")
	contract.Assertf(inputs != nil, "Construct requires input properties")

	label := fmt.Sprintf("%s.Construct(%s, %s, %s)", p.label(), typ, name, parent)
	logging.V(7).Infof("%s executing (#inputs=%v)", label, len(inputs))

	// Ensure that the plugin is configured.
	client := p.clientRaw
	pcfg, err := p.awaitConfig(context.Background())
	if err != nil {
		return ConstructResult{}, err
	}

	// We should only be calling Construct if the provider is fully configured.
	contract.Assertf(pcfg.known, "Construct cannot be called if the configuration is unknown")

	if !pcfg.acceptSecrets {
		return ConstructResult{}, fmt.Errorf("plugins that can construct components must support secrets")
	}

	// Marshal the input properties.
	minputs, err := MarshalProperties(inputs, MarshalOptions{
		Label:         fmt.Sprintf("%s.inputs", label),
		KeepUnknowns:  true,
		KeepSecrets:   pcfg.acceptSecrets,
		KeepResources: pcfg.acceptResources,
		// To initially scope the use of this new feature, we only keep output values for
		// Construct and Call (when the client accepts them).
		KeepOutputValues: pcfg.acceptOutputs,
	})
	if err != nil {
		return ConstructResult{}, err
	}

	// Marshal the aliases.
	aliasURNs := make([]string, len(options.Aliases))
	for i, alias := range options.Aliases {
		aliasURNs[i] = string(alias.URN)
	}

	// Marshal the dependencies.
	dependencies := make([]string, len(options.Dependencies))
	for i, dep := range options.Dependencies {
		dependencies[i] = string(dep)
	}

	// Marshal the property dependencies.
	inputDependencies := map[string]*pulumirpc.ConstructRequest_PropertyDependencies{}
	for name, dependencies := range options.PropertyDependencies {
		urns := make([]string, len(dependencies))
		for i, urn := range dependencies {
			urns[i] = string(urn)
		}
		inputDependencies[string(name)] = &pulumirpc.ConstructRequest_PropertyDependencies{Urns: urns}
	}

	// Marshal the config.
	config := map[string]string{}
	for k, v := range info.Config {
		config[k.String()] = v
	}
	configSecretKeys := []string{}
	for _, k := range info.ConfigSecretKeys {
		configSecretKeys = append(configSecretKeys, k.String())
	}

	req := &pulumirpc.ConstructRequest{
		Project:                 info.Project,
		Stack:                   info.Stack,
		Config:                  config,
		ConfigSecretKeys:        configSecretKeys,
		DryRun:                  info.DryRun,
		Parallel:                int32(info.Parallel),
		MonitorEndpoint:         info.MonitorAddress,
		Type:                    string(typ),
		Name:                    string(name),
		Parent:                  string(parent),
		Inputs:                  minputs,
		Protect:                 options.Protect,
		Providers:               options.Providers,
		InputDependencies:       inputDependencies,
		Aliases:                 aliasURNs,
		Dependencies:            dependencies,
		AdditionalSecretOutputs: options.AdditionalSecretOutputs,
		DeletedWith:             string(options.DeletedWith),
		DeleteBeforeReplace:     options.DeleteBeforeReplace,
		IgnoreChanges:           options.IgnoreChanges,
		ReplaceOnChanges:        options.ReplaceOnChanges,
		RetainOnDelete:          options.RetainOnDelete,
	}
	if ct := options.CustomTimeouts; ct != nil {
		req.CustomTimeouts = &pulumirpc.ConstructRequest_CustomTimeouts{
			Create: ct.Create,
			Update: ct.Update,
			Delete: ct.Delete,
		}
	}

	resp, err := client.Construct(p.requestContext(), req)
	if err != nil {
		return ConstructResult{}, err
	}

	outputs, err := UnmarshalProperties(resp.GetState(), MarshalOptions{
		Label:         fmt.Sprintf("%s.outputs", label),
		KeepUnknowns:  info.DryRun,
		KeepSecrets:   true,
		KeepResources: true,
	})
	if err != nil {
		return ConstructResult{}, err
	}

	outputDependencies := map[resource.PropertyKey][]resource.URN{}
	for k, rpcDeps := range resp.GetStateDependencies() {
		urns := make([]resource.URN, len(rpcDeps.Urns))
		for i, d := range rpcDeps.Urns {
			urns[i] = resource.URN(d)
		}
		outputDependencies[resource.PropertyKey(k)] = urns
	}

	logging.V(7).Infof("%s success: #outputs=%d", label, len(outputs))
	return ConstructResult{
		URN:                resource.URN(resp.GetUrn()),
		Outputs:            outputs,
		OutputDependencies: outputDependencies,
	}, nil
}

// Invoke dynamically executes a built-in function in the provider.
func (p *provider) Invoke(tok tokens.ModuleMember, args resource.PropertyMap) (resource.PropertyMap,
	[]CheckFailure, error,
) {
	contract.Assertf(tok != "", "Invoke requires a token")

	label := fmt.Sprintf("%s.Invoke(%s)", p.label(), tok)
	logging.V(7).Infof("%s executing (#args=%d)", label, len(args))

	// Ensure that the plugin is configured.
	client := p.clientRaw
	pcfg, err := p.awaitConfig(context.Background())
	if err != nil {
		return nil, nil, err
	}

	// If the provider is not fully configured, return an empty property map.
	if !pcfg.known {
		return resource.PropertyMap{}, nil, nil
	}

	margs, err := MarshalProperties(args, MarshalOptions{
		Label:         fmt.Sprintf("%s.args", label),
		KeepSecrets:   pcfg.acceptSecrets,
		KeepResources: pcfg.acceptResources,
	})
	if err != nil {
		return nil, nil, err
	}

	resp, err := client.Invoke(p.requestContext(), &pulumirpc.InvokeRequest{
		Tok:  string(tok),
		Args: margs,
	})
	if err != nil {
		rpcError := rpcerror.Convert(err)
		logging.V(7).Infof("%s failed: %v", label, rpcError.Message())
		return nil, nil, rpcError
	}

	// Unmarshal any return values.
	ret, err := UnmarshalProperties(resp.GetReturn(), MarshalOptions{
		Label:          fmt.Sprintf("%s.returns", label),
		RejectUnknowns: true,
		KeepSecrets:    true,
		KeepResources:  true,
	})
	if err != nil {
		return nil, nil, err
	}

	// And now any properties that failed verification.
	failures := make([]CheckFailure, 0, len(resp.GetFailures()))
	for _, failure := range resp.GetFailures() {
		failures = append(failures, CheckFailure{resource.PropertyKey(failure.Property), failure.Reason})
	}

	logging.V(7).Infof("%s success (#ret=%d,#failures=%d) success", label, len(ret), len(failures))
	return ret, failures, nil
}

// StreamInvoke dynamically executes a built-in function in the provider, which returns a stream of
// responses.
func (p *provider) StreamInvoke(
	tok tokens.ModuleMember,
	args resource.PropertyMap,
	onNext func(resource.PropertyMap) error,
) ([]CheckFailure, error) {
	contract.Assertf(tok != "", "StreamInvoke requires a token")

	label := fmt.Sprintf("%s.StreamInvoke(%s)", p.label(), tok)
	logging.V(7).Infof("%s executing (#args=%d)", label, len(args))

	// Ensure that the plugin is configured.
	client := p.clientRaw
	pcfg, err := p.awaitConfig(context.Background())
	if err != nil {
		return nil, err
	}

	// If the provider is not fully configured, return an empty property map.
	if !pcfg.known {
		return nil, onNext(resource.PropertyMap{})
	}

	margs, err := MarshalProperties(args, MarshalOptions{
		Label:         fmt.Sprintf("%s.args", label),
		KeepSecrets:   pcfg.acceptSecrets,
		KeepResources: pcfg.acceptResources,
	})
	if err != nil {
		return nil, err
	}

	streamClient, err := client.StreamInvoke(
		p.requestContext(), &pulumirpc.InvokeRequest{
			Tok:  string(tok),
			Args: margs,
		})
	if err != nil {
		rpcError := rpcerror.Convert(err)
		logging.V(7).Infof("%s failed: %v", label, rpcError.Message())
		return nil, rpcError
	}

	for {
		in, err := streamClient.Recv()
		if err == io.EOF {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}

		// Unmarshal response.
		ret, err := UnmarshalProperties(in.GetReturn(), MarshalOptions{
			Label:          fmt.Sprintf("%s.returns", label),
			RejectUnknowns: true,
			KeepSecrets:    true,
			KeepResources:  true,
		})
		if err != nil {
			return nil, err
		}

		// Check properties that failed verification.
		var failures []CheckFailure
		for _, failure := range in.GetFailures() {
			failures = append(failures, CheckFailure{resource.PropertyKey(failure.Property), failure.Reason})
		}

		if len(failures) > 0 {
			return failures, nil
		}

		// Send stream message back to whoever is consuming the stream.
		if err := onNext(ret); err != nil {
			return nil, err
		}
	}
}

// Call dynamically executes a method in the provider associated with a component resource.
func (p *provider) Call(tok tokens.ModuleMember, args resource.PropertyMap, info CallInfo,
	options CallOptions,
) (CallResult, error) {
	contract.Assertf(tok != "", "Call requires a token")

	label := fmt.Sprintf("%s.Call(%s)", p.label(), tok)
	logging.V(7).Infof("%s executing (#args=%d)", label, len(args))

	// Ensure that the plugin is configured.
	client := p.clientRaw
	pcfg, err := p.awaitConfig(context.Background())
	if err != nil {
		return CallResult{}, err
	}

	// If the provider is not fully configured, return an empty property map.
	if !pcfg.known {
		return CallResult{}, nil
	}

	margs, err := MarshalProperties(args, MarshalOptions{
		Label:         fmt.Sprintf("%s.args", label),
		KeepUnknowns:  true,
		KeepSecrets:   true,
		KeepResources: true,
		// To initially scope the use of this new feature, we only keep output values for
		// Construct and Call (when the client accepts them).
		KeepOutputValues: pcfg.acceptOutputs,
	})
	if err != nil {
		return CallResult{}, err
	}

	// Marshal the arg dependencies.
	argDependencies := map[string]*pulumirpc.CallRequest_ArgumentDependencies{}
	for name, dependencies := range options.ArgDependencies {
		urns := make([]string, len(dependencies))
		for i, urn := range dependencies {
			urns[i] = string(urn)
		}
		argDependencies[string(name)] = &pulumirpc.CallRequest_ArgumentDependencies{Urns: urns}
	}

	// Marshal the config.
	config := map[string]string{}
	for k, v := range info.Config {
		config[k.String()] = v
	}

	resp, err := client.Call(p.requestContext(), &pulumirpc.CallRequest{
		Tok:             string(tok),
		Args:            margs,
		ArgDependencies: argDependencies,
		Project:         info.Project,
		Stack:           info.Stack,
		Config:          config,
		DryRun:          info.DryRun,
		Parallel:        int32(info.Parallel),
		MonitorEndpoint: info.MonitorAddress,
	})
	if err != nil {
		rpcError := rpcerror.Convert(err)
		logging.V(7).Infof("%s failed: %v", label, rpcError.Message())
		return CallResult{}, rpcError
	}

	// Unmarshal any return values.
	ret, err := UnmarshalProperties(resp.GetReturn(), MarshalOptions{
		Label:         fmt.Sprintf("%s.returns", label),
		KeepUnknowns:  info.DryRun,
		KeepSecrets:   true,
		KeepResources: true,
	})
	if err != nil {
		return CallResult{}, err
	}

	returnDependencies := map[resource.PropertyKey][]resource.URN{}
	for k, rpcDeps := range resp.GetReturnDependencies() {
		urns := make([]resource.URN, len(rpcDeps.Urns))
		for i, d := range rpcDeps.Urns {
			urns[i] = resource.URN(d)
		}
		returnDependencies[resource.PropertyKey(k)] = urns
	}

	// And now any properties that failed verification.
	failures := make([]CheckFailure, 0, len(resp.GetFailures()))
	for _, failure := range resp.GetFailures() {
		failures = append(failures, CheckFailure{resource.PropertyKey(failure.Property), failure.Reason})
	}

	logging.V(7).Infof("%s success (#ret=%d,#failures=%d) success", label, len(ret), len(failures))
	return CallResult{Return: ret, ReturnDependencies: returnDependencies, Failures: failures}, nil
}

// GetPluginInfo returns this plugin's information.
func (p *provider) GetPluginInfo() (workspace.PluginInfo, error) {
	label := fmt.Sprintf("%s.GetPluginInfo()", p.label())
	logging.V(7).Infof("%s executing", label)

	// Calling GetPluginInfo happens immediately after loading, and does not require configuration to proceed.
	// Thus, we access the clientRaw property, rather than calling getClient.
	resp, err := p.clientRaw.GetPluginInfo(p.requestContext(), &pbempty.Empty{})
	if err != nil {
		rpcError := rpcerror.Convert(err)
		logging.V(7).Infof("%s failed: err=%v", label, rpcError.Message())
		return workspace.PluginInfo{}, rpcError
	}

	var version *semver.Version
	if v := resp.Version; v != "" {
		sv, err := semver.ParseTolerant(v)
		if err != nil {
			return workspace.PluginInfo{}, err
		}
		version = &sv
	}

	path := ""
	if p.plug != nil {
		path = p.plug.Bin
	}

	logging.V(7).Infof("%s success (#version=%v) success", label, version)
	return workspace.PluginInfo{
		Name:    string(p.pkg),
		Path:    path,
		Kind:    workspace.ResourcePlugin,
		Version: version,
	}, nil
}

// Attach attaches this plugin to the engine
func (p *provider) Attach(address string) error {
	label := fmt.Sprintf("%s.Attach()", p.label())
	logging.V(7).Infof("%s executing", label)

	// Calling Attach happens immediately after loading, and does not require configuration to proceed.
	// Thus, we access the clientRaw property, rather than calling getClient.
	_, err := p.clientRaw.Attach(p.requestContext(), &pulumirpc.PluginAttach{Address: address})
	if err != nil {
		rpcError := rpcerror.Convert(err)
		logging.V(7).Infof("%s failed: err=%v", label, rpcError.Message())
		return rpcError
	}

	return nil
}

func (p *provider) SignalCancellation() error {
	_, err := p.clientRaw.Cancel(p.requestContext(), &pbempty.Empty{})
	if err != nil {
		rpcError := rpcerror.Convert(err)
		logging.V(8).Infof("provider received rpc error `%s`: `%s`", rpcError.Code(),
			rpcError.Message())
		switch rpcError.Code() {
		case codes.Unimplemented:
			// For backwards compatibility, do nothing if it's not implemented.
			return nil
		}
	}

	return err
}

// Close tears down the underlying plugin RPC connection and process.
func (p *provider) Close() error {
	if p.plug == nil {
		return nil
	}
	return p.plug.Close()
}

// createConfigureError creates a nice error message from an RPC error that
// originated from `Configure`.
//
// If we requested that a resource configure itself but omitted required configuration
// variables, resource providers will respond with a list of missing variables and their descriptions.
// If that is what occurred, we'll use that information here to construct a nice error message.
func createConfigureError(rpcerr *rpcerror.Error) error {
	var err error
	for _, detail := range rpcerr.Details() {
		if missingKeys, ok := detail.(*pulumirpc.ConfigureErrorMissingKeys); ok {
			for _, missingKey := range missingKeys.MissingKeys {
				singleError := fmt.Errorf("missing required configuration key \"%s\": %s\n"+
					"Set a value using the command `pulumi config set %s <value>`.",
					missingKey.Name, missingKey.Description, missingKey.Name)
				err = multierror.Append(err, singleError)
			}
		}
	}

	if err != nil {
		return err
	}

	return rpcerr
}

// resourceStateAndError interprets an error obtained from a gRPC endpoint.
//
// gRPC gives us a `status.Status` structure as an `error` whenever our
// gRPC servers serve up an error. Each `status.Status` contains a code
// and a message. Based on the error code given to us, we can understand
// the state of our system and if our resource status is truly unknown.
//
// In general, our resource state is only really unknown if the server
// had an internal error, in which case it will serve one of `codes.Internal`,
// `codes.DataLoss`, or `codes.Unknown` to us.
func resourceStateAndError(err error) (resource.Status, *rpcerror.Error) {
	rpcError := rpcerror.Convert(err)
	logging.V(8).Infof("provider received rpc error `%s`: `%s`", rpcError.Code(), rpcError.Message())
	switch rpcError.Code() {
	case codes.Internal, codes.DataLoss, codes.Unknown:
		logging.V(8).Infof("rpc error kind `%s` may not be recoverable", rpcError.Code())
		return resource.StatusUnknown, rpcError
	}

	logging.V(8).Infof("rpc error kind `%s` is well-understood and recoverable", rpcError.Code())
	return resource.StatusOK, rpcError
}

// parseError parses a gRPC error into a set of values that represent the state of a resource. They
// are: (1) the `resourceStatus`, indicating the last known state (e.g., `StatusOK`, representing
// success, `StatusUnknown`, representing internal failure); (2) the `*rpcerror.Error`, our internal
// representation for RPC errors; and optionally (3) `liveObject`, containing the last known live
// version of the object that has successfully created but failed to initialize (e.g., because the
// object was created, but app code is continually crashing and the resource never achieves
// liveness).
func parseError(err error) (
	resourceStatus resource.Status, id resource.ID, liveInputs, liveObject *_struct.Struct, resourceErr error,
) {
	var responseErr *rpcerror.Error
	resourceStatus, responseErr = resourceStateAndError(err)
	contract.Assertf(responseErr != nil, "resourceStateAndError must never return a nil error")

	// If resource was successfully created but failed to initialize, the error will be packed
	// with the live properties of the object.
	resourceErr = responseErr
	for _, detail := range responseErr.Details() {
		if initErr, ok := detail.(*pulumirpc.ErrorResourceInitFailed); ok {
			id = resource.ID(initErr.GetId())
			liveObject = initErr.GetProperties()
			liveInputs = initErr.GetInputs()
			resourceStatus = resource.StatusPartialFailure
			resourceErr = &InitError{Reasons: initErr.Reasons}
			break
		}
	}

	return resourceStatus, id, liveObject, liveInputs, resourceErr
}

// InitError represents a failure to initialize a resource, i.e., the resource has been successfully
// created, but it has failed to initialize.
type InitError struct {
	Reasons []string
}

var _ error = (*InitError)(nil)

func (ie *InitError) Error() string {
	var err error
	for _, reason := range ie.Reasons {
		err = multierror.Append(err, errors.New(reason))
	}
	return err.Error()
}

func decorateSpanWithType(span opentracing.Span, urn string) {
	if urn := resource.URN(urn); urn.IsValid() {
		span.SetTag("pulumi-decorator", urn.Type())
	}
}

func decorateProviderSpans(span opentracing.Span, method string, req, resp interface{}, grpcError error) {
	if req == nil {
		return
	}

	switch method {
	case "/pulumirpc.ResourceProvider/Check", "/pulumirpc.ResourceProvider/CheckConfig":
		decorateSpanWithType(span, req.(*pulumirpc.CheckRequest).Urn)
	case "/pulumirpc.ResourceProvider/Diff", "/pulumirpc.ResourceProvider/DiffConfig":
		decorateSpanWithType(span, req.(*pulumirpc.DiffRequest).Urn)
	case "/pulumirpc.ResourceProvider/Create":
		decorateSpanWithType(span, req.(*pulumirpc.CreateRequest).Urn)
	case "/pulumirpc.ResourceProvider/Update":
		decorateSpanWithType(span, req.(*pulumirpc.UpdateRequest).Urn)
	case "/pulumirpc.ResourceProvider/Delete":
		decorateSpanWithType(span, req.(*pulumirpc.DeleteRequest).Urn)
	case "/pulumirpc.ResourceProvider/Invoke":
		span.SetTag("pulumi-decorator", req.(*pulumirpc.InvokeRequest).Tok)
	}
}

// GetMapping fetches the conversion mapping (if any) for this resource provider.
func (p *provider) GetMapping(key string) ([]byte, string, error) {
	label := fmt.Sprintf("%s.GetMapping", p.label())
	logging.V(7).Infof("%s executing: key=%s", label, key)

	resp, err := p.clientRaw.GetMapping(p.requestContext(), &pulumirpc.GetMappingRequest{
		Key: key,
	})
	if err != nil {
		rpcError := rpcerror.Convert(err)
		code := rpcError.Code()
		if code == codes.Unimplemented {
			// For backwards compatibility, just return nothing as if the provider didn't have a mapping for
			// the given key
			logging.V(7).Infof("%s unimplemented", label)
			return nil, "", nil
		}
		logging.V(7).Infof("%s failed: %v", label, rpcError)
		return nil, "", err
	}

	logging.V(7).Infof("%s success: data=#%d provider=%s", label, len(resp.Data), resp.Provider)
	return resp.Data, resp.Provider, nil
}
