/*
Copyright 2016 The Kubernetes Authors.

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

package set

import (
	"fmt"
	"io"

	"k8s.io/kubernetes/pkg/printers"

	"github.com/spf13/cobra"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/kubernetes/pkg/kubectl/cmd/templates"
	cmdutil "k8s.io/kubernetes/pkg/kubectl/cmd/util"
	"k8s.io/kubernetes/pkg/kubectl/resource"
	"k8s.io/kubernetes/pkg/kubectl/util/i18n"
)

// SelectorOptions is the start of the data required to perform the operation.  As new fields are added, add them here instead of
// referencing the cmd.Flags()
type SelectorOptions struct {
	fileOptions resource.FilenameOptions

	PrintFlags *printers.PrintFlags

	local       bool
	dryrun      bool
	all         bool
	record      bool
	changeCause string
	output      string

	resources []string
	selector  *metav1.LabelSelector

	out              io.Writer
	ClientForMapping func(mapping *meta.RESTMapping) (resource.RESTClient, error)

	PrintObj printers.ResourcePrinterFunc

	builder *resource.Builder
	mapper  meta.RESTMapper
}

var (
	selectorLong = templates.LongDesc(`
		Set the selector on a resource. Note that the new selector will overwrite the old selector if the resource had one prior to the invocation
		of 'set selector'.

		A selector must begin with a letter or number, and may contain letters, numbers, hyphens, dots, and underscores, up to %[1]d characters.
		If --resource-version is specified, then updates will use this resource version, otherwise the existing resource-version will be used.
        Note: currently selectors can only be set on Service objects.`)
	selectorExample = templates.Examples(`
        # set the labels and selector before creating a deployment/service pair.
        kubectl create service clusterip my-svc --clusterip="None" -o yaml --dry-run | kubectl set selector --local -f - 'environment=qa' -o yaml | kubectl create -f -
        kubectl create deployment my-dep -o yaml --dry-run | kubectl label --local -f - environment=qa -o yaml | kubectl create -f -`)
)

// NewCmdSelector is the "set selector" command.
func NewCmdSelector(f cmdutil.Factory, out io.Writer) *cobra.Command {
	options := &SelectorOptions{
		PrintFlags: printers.NewPrintFlags("selector updated"),

		out: out,
	}

	cmd := &cobra.Command{
		Use: "selector (-f FILENAME | TYPE NAME) EXPRESSIONS [--resource-version=version]",
		DisableFlagsInUseLine: true,
		Short:   i18n.T("Set the selector on a resource"),
		Long:    fmt.Sprintf(selectorLong, validation.LabelValueMaxLength),
		Example: selectorExample,
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(options.Complete(f, cmd, args))
			cmdutil.CheckErr(options.Validate())
			cmdutil.CheckErr(options.RunSelector())
		},
	}

	options.PrintFlags.AddFlags(cmd)

	cmd.Flags().Bool("all", false, "Select all resources, including uninitialized ones, in the namespace of the specified resource types")
	cmd.Flags().Bool("local", false, "If true, set selector will NOT contact api-server but run locally.")
	cmd.Flags().String("resource-version", "", "If non-empty, the selectors update will only succeed if this is the current resource-version for the object. Only valid when specifying a single resource.")
	usage := "the resource to update the selectors"
	cmdutil.AddFilenameOptionFlags(cmd, &options.fileOptions, usage)
	cmdutil.AddDryRunFlag(cmd)
	cmdutil.AddRecordFlag(cmd)
	cmdutil.AddIncludeUninitializedFlag(cmd)

	return cmd
}

// Complete assigns the SelectorOptions from args.
func (o *SelectorOptions) Complete(f cmdutil.Factory, cmd *cobra.Command, args []string) error {
	o.local = cmdutil.GetFlagBool(cmd, "local")
	o.all = cmdutil.GetFlagBool(cmd, "all")
	o.record = cmdutil.GetRecordFlag(cmd)
	o.dryrun = cmdutil.GetDryRunFlag(cmd)
	o.output = cmdutil.GetFlagString(cmd, "output")

	cmdNamespace, enforceNamespace, err := f.DefaultNamespace()
	if err != nil {
		return err
	}

	o.changeCause = f.Command(cmd, false)
	mapper, _ := f.Object()
	o.mapper = mapper

	o.resources, o.selector, err = getResourcesAndSelector(args)
	if err != nil {
		return err
	}

	includeUninitialized := cmdutil.ShouldIncludeUninitialized(cmd, false)
	o.builder = f.NewBuilder().
		Internal().
		LocalParam(o.local).
		ContinueOnError().
		NamespaceParam(cmdNamespace).DefaultNamespace().
		FilenameParam(enforceNamespace, &o.fileOptions).
		IncludeUninitialized(includeUninitialized).
		Flatten()

	if !o.local {
		o.builder.
			ResourceTypeOrNameArgs(o.all, o.resources...).
			Latest()
	} else {
		// if a --local flag was provided, and a resource was specified in the form
		// <resource>/<name>, fail immediately as --local cannot query the api server
		// for the specified resource.
		if len(o.resources) > 0 {
			return resource.LocalResourceError
		}
	}

	if o.dryrun {
		o.PrintFlags.Complete("%s (dry run)")
	}
	printer, err := o.PrintFlags.ToPrinter()
	if err != nil {
		return err
	}
	o.PrintObj = printer.PrintObj

	o.ClientForMapping = func(mapping *meta.RESTMapping) (resource.RESTClient, error) {
		return f.ClientForMapping(mapping)
	}
	return err
}

// Validate basic inputs
func (o *SelectorOptions) Validate() error {
	if len(o.resources) < 1 && cmdutil.IsFilenameSliceEmpty(o.fileOptions.Filenames) {
		return fmt.Errorf("one or more resources must be specified as <resource> <name> or <resource>/<name>")
	}
	if o.selector == nil {
		return fmt.Errorf("one selector is required")
	}
	return nil
}

// RunSelector executes the command.
func (o *SelectorOptions) RunSelector() error {
	r := o.builder.Do()
	err := r.Err()
	if err != nil {
		return err
	}

	return r.Visit(func(info *resource.Info, err error) error {
		patch := &Patch{Info: info}
		CalculatePatch(patch, cmdutil.InternalVersionJSONEncoder(), func(info *resource.Info) ([]byte, error) {
			versioned := info.AsVersioned()
			patch.Info.Object = versioned
			selectErr := updateSelectorForObject(info.Object, *o.selector)

			if selectErr == nil {
				return runtime.Encode(cmdutil.InternalVersionJSONEncoder(), info.Object)
			}
			return nil, selectErr
		})

		if patch.Err != nil {
			return patch.Err
		}
		if o.local || o.dryrun {
			return o.PrintObj(info.Object, o.out)
		}

		patched, err := resource.NewHelper(info.Client, info.Mapping).Patch(info.Namespace, info.Name, types.StrategicMergePatchType, patch.Patch)
		if err != nil {
			return err
		}

		if o.record || cmdutil.ContainsChangeCause(info) {
			if err := cmdutil.RecordChangeCause(patched, o.changeCause); err == nil {
				if patched, err = resource.NewHelper(info.Client, info.Mapping).Replace(info.Namespace, info.Name, false, patched); err != nil {
					return fmt.Errorf("changes to %s/%s can't be recorded: %v\n", info.Mapping.Resource, info.Name, err)
				}
			}
		}

		info.Refresh(patched, true)
		return o.PrintObj(patch.Info.AsVersioned(), o.out)
	})
}

func updateSelectorForObject(obj runtime.Object, selector metav1.LabelSelector) error {
	copyOldSelector := func() (map[string]string, error) {
		if len(selector.MatchExpressions) > 0 {
			return nil, fmt.Errorf("match expression %v not supported on this object", selector.MatchExpressions)
		}
		dst := make(map[string]string)
		for label, value := range selector.MatchLabels {
			dst[label] = value
		}
		return dst, nil
	}
	var err error
	switch t := obj.(type) {
	case *v1.Service:
		t.Spec.Selector, err = copyOldSelector()
	default:
		err = fmt.Errorf("setting a selector is only supported for Services")
	}
	return err
}

// getResourcesAndSelector retrieves resources and the selector expression from the given args (assuming selectors the last arg)
func getResourcesAndSelector(args []string) (resources []string, selector *metav1.LabelSelector, err error) {
	if len(args) == 0 {
		return []string{}, nil, nil
	}
	resources = args[:len(args)-1]
	selector, err = metav1.ParseToLabelSelector(args[len(args)-1])
	return resources, selector, err
}
