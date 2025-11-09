package proxmox

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
)

type ConfigSnapshot struct {
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	VmState     bool   `json:"ram,omitempty"`
}

func (config *ConfigSnapshot) mapToApiValues() map[string]interface{} {
	return map[string]interface{}{
		"snapname":    config.Name,
		"description": config.Description,
		"vmstate":     config.VmState,
	}
}

func (config *ConfigSnapshot) CreateSnapshot(ctx context.Context, c *Client, guestId uint) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	params := config.mapToApiValues()
	vmr := NewVmRef(int(guestId))
	_, err = c.GetVmInfo(ctx, vmr)
	if err != nil {
		return
	}
	_, err = c.PostWithTask(ctx, params, "/nodes/"+vmr.node+"/"+vmr.vmType+"/"+strconv.Itoa(vmr.vmId)+"/snapshot/")
	if err != nil {
		params, _ := json.Marshal(&params)
		return fmt.Errorf("error creating Snapshot: %v, (params: %v)", err, string(params))
	}
	return
}

func ListSnapshots(ctx context.Context, c *Client, vmr *VmRef) (taskResponse []interface{}, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	err = c.CheckVmRef(ctx, vmr)
	if err != nil {
		return
	}
	return c.GetItemConfigInterfaceArray(ctx, "/nodes/"+vmr.node+"/"+vmr.vmType+"/"+strconv.Itoa(vmr.vmId)+"/snapshot/", "Guest", "SNAPSHOTS")
}

// Can only be used to update the description of an already existing snapshot
func UpdateSnapshotDescription(ctx context.Context, c *Client, vmr *VmRef, snapshot, description string) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	err = c.CheckVmRef(ctx, vmr)
	if err != nil {
		return
	}
	return c.Put(ctx, map[string]interface{}{"description": description}, "/nodes/"+vmr.node+"/"+vmr.vmType+"/"+strconv.Itoa(vmr.vmId)+"/snapshot/"+snapshot+"/config")
}

func DeleteSnapshot(ctx context.Context, c *Client, vmr *VmRef, snapshot string) (exitStatus string, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	err = c.CheckVmRef(ctx, vmr)
	if err != nil {
		return
	}
	return c.DeleteWithTask(ctx, "/nodes/" + vmr.node + "/" + vmr.vmType + "/" + strconv.Itoa(vmr.vmId) + "/snapshot/" + snapshot)
}

func RollbackSnapshot(ctx context.Context, c *Client, vmr *VmRef, snapshot string) (exitStatus string, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	err = c.CheckVmRef(ctx, vmr)
	if err != nil {
		return
	}
	return c.PostWithTask(ctx, nil, "/nodes/"+vmr.node+"/"+vmr.vmType+"/"+strconv.Itoa(vmr.vmId)+"/snapshot/"+snapshot+"/rollback")
}

// Used for formatting the output when retrieving snapshots
type Snapshot struct {
	Name        string      `json:"name"`
	SnapTime    uint        `json:"time,omitempty"`
	Description string      `json:"description,omitempty"`
	VmState     bool        `json:"ram,omitempty"`
	Children    []*Snapshot `json:"children,omitempty"`
	Parent      string      `json:"parent,omitempty"`
}

// Formats the taskResponse as a list of snapshots
func FormatSnapshotsList(taskResponse []interface{}) (list []*Snapshot) {
	list = make([]*Snapshot, len(taskResponse))
	for i, e := range taskResponse {
		list[i] = &Snapshot{}
		if _, isSet := e.(map[string]interface{})["description"]; isSet {
			list[i].Description = e.(map[string]interface{})["description"].(string)
		}
		if _, isSet := e.(map[string]interface{})["name"]; isSet {
			list[i].Name = e.(map[string]interface{})["name"].(string)
		}
		if _, isSet := e.(map[string]interface{})["parent"]; isSet {
			list[i].Parent = e.(map[string]interface{})["parent"].(string)
		}
		if _, isSet := e.(map[string]interface{})["snaptime"]; isSet {
			list[i].SnapTime = uint(e.(map[string]interface{})["snaptime"].(float64))
		}
		if _, isSet := e.(map[string]interface{})["vmstate"]; isSet {
			list[i].VmState = Itob(int(e.(map[string]interface{})["vmstate"].(float64)))
		}
	}
	return
}

// Formats a list of snapshots as a tree of snapshots
func FormatSnapshotsTree(taskResponse []interface{}) (tree []*Snapshot) {
	list := FormatSnapshotsList(taskResponse)
	for _, e := range list {
		for _, ee := range list {
			if e.Parent == ee.Name {
				ee.Children = append(ee.Children, e)
				break
			}
		}
	}
	for _, e := range list {
		if e.Parent == "" {
			tree = append(tree, e)
		}
		e.Parent = ""
	}
	return
}
