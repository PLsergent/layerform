package command

import (
	"context"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/terraform-exec/tfexec"
	"github.com/pkg/errors"

	"github.com/ergomake/layerform/internal/data/model"
	"github.com/ergomake/layerform/internal/layers"
	"github.com/ergomake/layerform/internal/layerstate"
	"github.com/ergomake/layerform/internal/terraform"
)

type killCommand struct {
	layersBackend layers.Backend
	statesBackend layerstate.Backend
}

func NewKill(layersBackend layers.Backend, statesBackend layerstate.Backend) *killCommand {
	return &killCommand{layersBackend, statesBackend}
}

func (c *killCommand) Run(ctx context.Context, layerName, stateName string, vars []string) error {
	logger := hclog.FromContext(ctx)

	layer, err := c.layersBackend.GetLayer(ctx, layerName)
	if err != nil {
		return errors.Wrap(err, "fail to get layer")
	}

	if layer == nil {
		return errors.New("layer not found")
	}

	state, err := c.statesBackend.GetState(ctx, layer.Name, stateName)
	if err != nil {
		if errors.Is(err, layerstate.ErrStateNotFound) {
			return errors.Errorf(
				"state %s not found for layer %s\n",
				stateName,
				layer.Name,
			)
		}

		return errors.Wrap(err, "fail to get layer state")
	}

	hasDependants, err := c.hasDependants(ctx, layerName, stateName)
	if err != nil {
		return errors.Wrap(err, "fail to check if layer has dependants")
	}
	if hasDependants {
		return errors.New("can't kill this layer because other layers depend on it")
	}

	tfpath, err := terraform.GetTFPath(ctx)
	if err != nil {
		return errors.Wrap(err, "fail to get terraform path")
	}
	logger.Debug("Using terraform from", "tfpath", tfpath)
	logger.Debug("Found terraform installation", "tfpath", tfpath)

	logger.Debug("Creating a temporary work directory")
	workdir, err := os.MkdirTemp("", "")
	if err != nil {
		return errors.Wrap(err, "fail to create work directory")
	}
	fmt.Println(workdir)
	defer os.RemoveAll(workdir)

	layerDir := path.Join(workdir, layerName)
	layerAddrs, layerDir, err := c.getLayerAddresses(ctx, layer, state, layerDir, tfpath)
	if err != nil {
		return errors.Wrap(err, "fail to get layer addresses")
	}

	layerAddrsMap := make(map[string]struct{})
	for _, addr := range layerAddrs {
		layerAddrsMap[addr] = struct{}{}
	}

	for _, dep := range layer.Dependencies {
		depLayer, err := c.layersBackend.GetLayer(ctx, dep)
		if err != nil {
			return errors.Wrap(err, "fail to get dependency layer")
		}

		if depLayer == nil {
			return errors.Wrap(err, "dependency layer not found")
		}

		depState, err := c.statesBackend.GetState(ctx, depLayer.Name, state.GetDependencyStateName(dep))
		if err != nil {
			return errors.Wrap(err, "fail to get dependency state")
		}

		depDir := path.Join(workdir, dep)
		depAddrs, _, err := c.getLayerAddresses(ctx, depLayer, depState, depDir, tfpath)
		if err != nil {
			return errors.Wrap(err, "fail to get dependency layer addresses")
		}

		for _, addr := range depAddrs {
			delete(layerAddrsMap, addr)
		}
	}

	tf, err := tfexec.NewTerraform(layerDir, tfpath)
	if err != nil {
		return errors.Wrap(err, "fail to get terraform client")
	}

	logger.Debug("Looking for variable definitions in .tfvars files")
	varFiles, err := findTFVarFiles()
	if err != nil {
		return errors.Wrap(err, "fail to find .tfvars files")
	}
	logger.Debug(fmt.Sprintf("Found %d var files", len(varFiles)), "varFiles", varFiles)

	destroyOptions := make([]tfexec.DestroyOption, 0)
	for _, vf := range varFiles {
		destroyOptions = append(destroyOptions, tfexec.VarFile(vf))
	}
	for _, v := range vars {
		destroyOptions = append(destroyOptions, tfexec.Var(v))
	}

	for addr := range layerAddrsMap {
		destroyOptions = append(destroyOptions, tfexec.Target(addr))
	}
	logger.Debug(
		"Running terraform destroy targetting layer specific addresses",
		"layer", layer.Name, "state", stateName, "targets", destroyOptions,
	)

	var answer string
	fmt.Printf("Deleting %s.%s. This can't be undone. Are you sure? [yes/no]: ", layerName, stateName)
	_, err = fmt.Scan(&answer)
	if err != nil {
		return errors.Wrap(err, "fail to read asnwer")
	}

	if strings.ToLower(strings.TrimSpace(answer)) != "yes" {
		return nil
	}

	err = tf.Destroy(ctx, destroyOptions...)
	if err != nil {
		return errors.Wrap(err, "fail to terraform destroy")
	}

	err = c.statesBackend.DeleteState(ctx, layerName, stateName)
	if err != nil {
		return errors.Wrap(err, "fail to delete state")
	}

	return nil
}

func (c *killCommand) computeStateByLayer(ctx context.Context, layer *model.Layer, state *layerstate.State) (map[string]string, error) {
	stateByLayer := map[string]string{}
	stateByLayer[layer.Name] = state.StateName
	for _, dep := range layer.Dependencies {
		depLayer, err := c.layersBackend.GetLayer(ctx, dep)
		if err != nil {
			return nil, errors.Wrap(err, "fail to get layer")
		}

		depStateName := state.GetDependencyStateName(dep)

		depState, err := c.statesBackend.GetState(ctx, dep, depStateName)
		if err != nil {
			return nil, errors.Wrap(err, "fail to get state")
		}

		depDepStates, err := c.computeStateByLayer(ctx, depLayer, depState)
		if err != nil {
			return nil, errors.Wrap(err, "fail to compute state by layer")
		}

		for k, v := range depDepStates {
			stateByLayer[k] = v
		}

		stateByLayer[dep] = depStateName
	}

	return stateByLayer, nil
}

func (c *killCommand) getLayerAddresses(
	ctx context.Context,
	layer *model.Layer,
	state *layerstate.State,
	layerDir, tfpath string,
) ([]string, string, error) {
	logger := hclog.FromContext(ctx)
	logger.Debug("Getting layer addresses", "layer", layer.Name, "state", state.StateName)

	stateByLayer, err := c.computeStateByLayer(ctx, layer, state)
	if err != nil {
		return nil, "", errors.Wrap(err, "fail to compute state by layer state")
	}

	layerWorkdir, err := writeLayerToWorkdir(ctx, c.layersBackend, layerDir, layer, stateByLayer)
	if err != nil {
		return nil, "", errors.Wrap(err, "fail to write layer to work directory")
	}

	statePath := path.Join(layerWorkdir, "terraform.tfstate")
	err = os.WriteFile(statePath, state.Bytes, 0644)
	if err != nil {
		return nil, "", errors.Wrap(err, "fail to write terraform state to work directory")
	}

	tf, err := tfexec.NewTerraform(layerWorkdir, tfpath)
	if err != nil {
		return nil, "", errors.Wrap(err, "fail to get terraform client")
	}

	logger.Debug("Running terraform init", "layer", layer.Name, "state", state.StateName)
	err = tf.Init(ctx)
	if err != nil {
		return nil, "", errors.Wrap(err, "fail to terraform init")
	}

	tfState, err := getTFState(ctx, statePath, tfpath)
	if err != nil {
		return nil, "", errors.Wrap(err, "fail to get terraform state")
	}

	addresses := getStateModuleAddresses(tfState.Values.RootModule)

	return addresses, layerWorkdir, nil
}

func (c *killCommand) hasDependants(ctx context.Context, layerName, stateName string) (bool, error) {
	hclog.FromContext(ctx).Debug("Checking if layer has dependants", "layer", layerName, "state", stateName)

	layers, err := c.layersBackend.ListLayers(ctx)
	if err != nil {
		return false, errors.Wrap(err, "fail to list layers")
	}

	for _, layer := range layers {
		isChild := false
		for _, d := range layer.Dependencies {
			if d == layerName {
				isChild = true
				break
			}
		}

		if isChild {
			states, err := c.statesBackend.ListStatesByLayer(ctx, layer.Name)
			if err != nil {
				return false, errors.Wrap(err, "fail to list layer states")
			}

			for _, state := range states {
				parentStateName := state.GetDependencyStateName(layerName)
				if parentStateName == stateName {
					return true, nil
				}
			}
		}
	}

	return false, nil
}
