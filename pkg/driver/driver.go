// Package driver implements the CSI Identity, Controller and Node services
// against the E2E Networks block-storage API.
package driver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	mountutils "k8s.io/mount-utils"
	utilexec "k8s.io/utils/exec"

	"github.com/yashgoyal40/csi-e2enetworks/pkg/e2e"
)

const (
	// PluginName is the CSI driver name; storage classes reference this.
	PluginName = "csi.e2enetworks.com"
	Version    = "0.1.4"

	minVolumeBytes = 100 * 1024 * 1024 * 1024 // 100 GB minimum tier on E2E
	defaultFSType  = "ext4"

	// hostPath bind: containerd mounts its own /sys over the pod's /sys, so
	// the chart's hostPath:/sys volume doesn't take effect — pods see only
	// loop devices in /sys/block. We also mount the host root at /host, so
	// the real /sys lives at /host/sys inside the pod and works reliably.
	sysBlockDir   = "/host/sys/block"
	pciRescanFile = "/host/sys/bus/pci/rescan"
)

// Driver bundles all three CSI servers behind a single struct.
type Driver struct {
	csi.UnimplementedIdentityServer
	csi.UnimplementedControllerServer
	csi.UnimplementedNodeServer

	api      *e2e.Client
	mounter  mountutils.Interface
	nodeIP   string // private IP of the node this binary runs on (Node mode)
	nodeName string // hostname (Node mode); used as CSI nodeID for friendliness

	// in-memory cache: kubernetes-hostname → e2e block-storage vm_id
	mu       sync.Mutex
	vmIDByIP map[string]int
}

// New constructs a Driver. nodeIP/nodeName are required only for Node mode.
func New(api *e2e.Client, nodeIP, nodeName string) *Driver {
	return &Driver{
		api:      api,
		mounter:  mountutils.New(""),
		nodeIP:   nodeIP,
		nodeName: nodeName,
		vmIDByIP: map[string]int{},
	}
}

// ===== Identity ===============================================================

func (d *Driver) GetPluginInfo(ctx context.Context, _ *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{Name: PluginName, VendorVersion: Version}, nil
}

func (d *Driver) GetPluginCapabilities(ctx context.Context, _ *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	return &csi.GetPluginCapabilitiesResponse{
		Capabilities: []*csi.PluginCapability{
			{
				Type: &csi.PluginCapability_Service_{
					Service: &csi.PluginCapability_Service{Type: csi.PluginCapability_Service_CONTROLLER_SERVICE},
				},
			},
		},
	}, nil
}

func (d *Driver) Probe(ctx context.Context, _ *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{}, nil
}

// ===== Controller =============================================================

func (d *Driver) ControllerGetCapabilities(ctx context.Context, _ *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	caps := []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
		csi.ControllerServiceCapability_RPC_LIST_VOLUMES,
	}
	out := make([]*csi.ControllerServiceCapability, 0, len(caps))
	for _, c := range caps {
		out = append(out, &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{Rpc: &csi.ControllerServiceCapability_RPC{Type: c}},
		})
	}
	return &csi.ControllerGetCapabilitiesResponse{Capabilities: out}, nil
}

func (d *Driver) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "Name is required")
	}
	sizeBytes := int64(minVolumeBytes)
	if r := req.GetCapacityRange(); r != nil {
		if r.RequiredBytes > sizeBytes {
			sizeBytes = r.RequiredBytes
		}
		if r.LimitBytes > 0 && r.LimitBytes < sizeBytes {
			return nil, status.Errorf(codes.OutOfRange, "limit %d < required %d", r.LimitBytes, sizeBytes)
		}
	}
	sizeGB := int((sizeBytes + (1 << 30) - 1) >> 30) // round up
	if sizeGB < 100 {
		sizeGB = 100
	}

	// IOPS tier defaults to 1500 for 100 GB; scale linearly. Param override possible.
	iops := 1500
	if v, ok := req.Parameters["iops"]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			iops = n
		}
	}

	// Idempotency: re-use an existing volume with the same name.
	vols, err := d.api.ListVolumes(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list volumes: %v", err)
	}
	var v *e2e.Volume
	for i := range vols {
		if vols[i].Name == req.Name {
			vv := vols[i]
			v = &vv
			break
		}
	}
	if v == nil {
		v, err = d.api.CreateVolume(ctx, req.Name, sizeGB, iops)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "create: %v", err)
		}
	}

	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      strconv.Itoa(v.BlockID),
			CapacityBytes: int64(sizeGB) * (1 << 30),
			VolumeContext: map[string]string{
				"name":   v.Name,
				"sizeGB": strconv.Itoa(sizeGB),
			},
		},
	}, nil
}

func (d *Driver) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	id, err := strconv.Atoi(req.VolumeId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "VolumeId must be an integer")
	}
	if err := d.api.DeleteVolume(ctx, id); err != nil {
		// 404 → already gone, treat as success.
		if isNotFoundErr(err) {
			return &csi.DeleteVolumeResponse{}, nil
		}
		return nil, status.Errorf(codes.Internal, "delete: %v", err)
	}
	return &csi.DeleteVolumeResponse{}, nil
}

func (d *Driver) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	volID, err := strconv.Atoi(req.VolumeId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "VolumeId must be an integer")
	}
	nodeIP := req.NodeId // we set NodeID = private IP in NodeGetInfo
	if nodeIP == "" {
		return nil, status.Error(codes.InvalidArgument, "NodeId is required")
	}
	vmID, err := d.lookupVMID(ctx, volID, nodeIP)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "vm lookup: %v", err)
	}
	if err := d.api.AttachVolume(ctx, volID, vmID); err != nil {
		// "already attached" comes back via API code; treat 412 with same VM as ok
		if isAlreadyAttached(err) {
			// fall through
		} else {
			return nil, status.Errorf(codes.Internal, "attach: %v", err)
		}
	}
	return &csi.ControllerPublishVolumeResponse{
		PublishContext: map[string]string{
			"volumeId": req.VolumeId,
			"vmID":     strconv.Itoa(vmID),
		},
	}, nil
}

func (d *Driver) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	volID, err := strconv.Atoi(req.VolumeId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "VolumeId must be an integer")
	}
	nodeIP := req.NodeId
	if nodeIP == "" {
		// CSI: empty NodeId means "detach from any" — fall back to current attachment.
		v, err := d.api.GetVolume(ctx, volID)
		if err != nil {
			if isNotFoundErr(err) {
				return &csi.ControllerUnpublishVolumeResponse{}, nil
			}
			return nil, status.Errorf(codes.Internal, "get volume: %v", err)
		}
		if v.VMDetail.VMID == 0 {
			return &csi.ControllerUnpublishVolumeResponse{}, nil
		}
		if err := d.api.DetachVolume(ctx, volID, v.VMDetail.VMID); err != nil {
			return nil, status.Errorf(codes.Internal, "detach: %v", err)
		}
	} else {
		vmID, err := d.lookupVMID(ctx, volID, nodeIP)
		if err != nil {
			return nil, status.Errorf(codes.NotFound, "vm lookup: %v", err)
		}
		if err := d.api.DetachVolume(ctx, volID, vmID); err != nil {
			return nil, status.Errorf(codes.Internal, "detach: %v", err)
		}
	}

	// Wait for status=Available so subsequent attach (e.g. on another node) is clean.
	wait, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if _, err := d.api.WaitForStatus(wait, volID, "Available", 3*time.Second); err != nil {
		return nil, status.Errorf(codes.Aborted, "wait detach: %v", err)
	}
	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

func (d *Driver) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	for _, c := range req.VolumeCapabilities {
		if c.GetMount() == nil && c.GetBlock() == nil {
			return &csi.ValidateVolumeCapabilitiesResponse{Message: "missing access type"}, nil
		}
		if c.AccessMode == nil || c.AccessMode.Mode != csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER {
			return &csi.ValidateVolumeCapabilitiesResponse{Message: "only RWO supported"}, nil
		}
	}
	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{VolumeCapabilities: req.VolumeCapabilities},
	}, nil
}

func (d *Driver) ListVolumes(ctx context.Context, _ *csi.ListVolumesRequest) (*csi.ListVolumesResponse, error) {
	vs, err := d.api.ListVolumes(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list: %v", err)
	}
	out := make([]*csi.ListVolumesResponse_Entry, 0, len(vs))
	for _, v := range vs {
		out = append(out, &csi.ListVolumesResponse_Entry{
			Volume: &csi.Volume{
				VolumeId:      strconv.Itoa(v.BlockID),
				CapacityBytes: int64(v.SizeMiB) * (1 << 20),
			},
		})
	}
	return &csi.ListVolumesResponse{Entries: out}, nil
}

// lookupVMID resolves a node IP to the block-storage vm_id, with caching.
// Pass anyVolumeID = a volume id (since EligibleVMs is per-volume but the
// list is identical across volumes in the same project).
func (d *Driver) lookupVMID(ctx context.Context, anyVolumeID int, nodeIP string) (int, error) {
	d.mu.Lock()
	if v, ok := d.vmIDByIP[nodeIP]; ok {
		d.mu.Unlock()
		return v, nil
	}
	d.mu.Unlock()

	id, err := d.api.VMIDByIP(ctx, anyVolumeID, nodeIP)
	if err != nil {
		return 0, err
	}
	d.mu.Lock()
	d.vmIDByIP[nodeIP] = id
	d.mu.Unlock()
	return id, nil
}

// ===== Node ===================================================================

func (d *Driver) NodeGetCapabilities(ctx context.Context, _ *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	caps := []csi.NodeServiceCapability_RPC_Type{
		csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
	}
	out := make([]*csi.NodeServiceCapability, 0, len(caps))
	for _, c := range caps {
		out = append(out, &csi.NodeServiceCapability{
			Type: &csi.NodeServiceCapability_Rpc{Rpc: &csi.NodeServiceCapability_RPC{Type: c}},
		})
	}
	return &csi.NodeGetCapabilitiesResponse{Capabilities: out}, nil
}

func (d *Driver) NodeGetInfo(ctx context.Context, _ *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	if d.nodeIP == "" {
		return nil, status.Error(codes.FailedPrecondition, "E2E_NODE_IP not set")
	}
	return &csi.NodeGetInfoResponse{
		NodeId:            d.nodeIP, // we use the private IP as the CSI nodeID
		MaxVolumesPerNode: 16,
	}, nil
}

func (d *Driver) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	if req.StagingTargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "StagingTargetPath required")
	}

	// Pre-attach snapshot of /sys/block (used to identify the new device after rescan).
	pre, err := snapshotBlockDevs()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "block snapshot: %v", err)
	}

	// Force PCI rescan in case kubelet got us here without one.
	_ = os.WriteFile(pciRescanFile, []byte("1"), 0)

	// Wait for a new device. Up to 30 s.
	deadline := time.Now().Add(30 * time.Second)
	var devPath string
	for time.Now().Before(deadline) {
		post, err := snapshotBlockDevs()
		if err != nil {
			return nil, status.Errorf(codes.Internal, "block snapshot: %v", err)
		}
		if d := newestNew(pre, post); d != "" {
			devPath = "/dev/" + d
			break
		}
		_ = os.WriteFile(pciRescanFile, []byte("1"), 0)
		time.Sleep(1 * time.Second)
	}
	if devPath == "" {
		return nil, status.Error(codes.Aborted, "no new block device appeared within 30s after attach")
	}

	if err := os.MkdirAll(req.StagingTargetPath, 0750); err != nil {
		return nil, status.Errorf(codes.Internal, "mkdir staging: %v", err)
	}

	fsType := defaultFSType
	if mc := req.VolumeCapability.GetMount(); mc != nil && mc.FsType != "" {
		fsType = mc.FsType
	}

	formatter := mountutils.NewSafeFormatAndMount(d.mounter, utilexec.New())
	if err := formatter.FormatAndMount(devPath, req.StagingTargetPath, fsType, mountOptions(req.VolumeCapability)); err != nil {
		return nil, status.Errorf(codes.Internal, "format+mount %s -> %s: %v", devPath, req.StagingTargetPath, err)
	}

	return &csi.NodeStageVolumeResponse{}, nil
}

func (d *Driver) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	if req.StagingTargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "StagingTargetPath required")
	}
	if err := mountutils.CleanupMountPoint(req.StagingTargetPath, d.mounter, true); err != nil {
		return nil, status.Errorf(codes.Internal, "unmount staging: %v", err)
	}
	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (d *Driver) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	if req.StagingTargetPath == "" || req.TargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "Staging+TargetPath required")
	}
	if err := os.MkdirAll(req.TargetPath, 0750); err != nil {
		return nil, status.Errorf(codes.Internal, "mkdir target: %v", err)
	}
	opts := []string{"bind"}
	if req.Readonly {
		opts = append(opts, "ro")
	}
	if err := d.mounter.Mount(req.StagingTargetPath, req.TargetPath, "", opts); err != nil {
		return nil, status.Errorf(codes.Internal, "bind-mount: %v", err)
	}
	return &csi.NodePublishVolumeResponse{}, nil
}

func (d *Driver) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	if req.TargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "TargetPath required")
	}
	if err := mountutils.CleanupMountPoint(req.TargetPath, d.mounter, true); err != nil {
		return nil, status.Errorf(codes.Internal, "unmount target: %v", err)
	}
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// ===== helpers ================================================================

// snapshotBlockDevs returns the set of block-device names under /sys/block.
// We only care about ones whose names start with vd or sd (real disks).
func snapshotBlockDevs() (map[string]uint64, error) {
	ents, err := os.ReadDir(sysBlockDir)
	if err != nil {
		return nil, err
	}
	out := map[string]uint64{}
	for _, e := range ents {
		n := e.Name()
		if !strings.HasPrefix(n, "vd") && !strings.HasPrefix(n, "sd") {
			continue
		}
		seq, err := readUint64(filepath.Join(sysBlockDir, n, "diskseq"))
		if err != nil {
			seq = 0
		}
		out[n] = seq
	}
	return out, nil
}

// newestNew returns the post-set name with the highest diskseq among entries
// that aren't in pre. Empty if no new device.
func newestNew(pre, post map[string]uint64) string {
	type item struct {
		name string
		seq  uint64
	}
	var diff []item
	for n, s := range post {
		if _, had := pre[n]; !had {
			diff = append(diff, item{n, s})
		}
	}
	if len(diff) == 0 {
		return ""
	}
	sort.Slice(diff, func(i, j int) bool { return diff[i].seq > diff[j].seq })
	return diff[0].name
}

func readUint64(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}

func mountOptions(c *csi.VolumeCapability) []string {
	if mc := c.GetMount(); mc != nil {
		return append([]string{}, mc.MountFlags...)
	}
	return nil
}

func isNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "404") || strings.Contains(strings.ToLower(s), "not found")
}

func isAlreadyAttached(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "already attached")
}

var _ = errors.New // keep import for future
