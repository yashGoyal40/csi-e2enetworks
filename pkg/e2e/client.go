// Package e2e is a minimal HTTP client for the E2E Networks block-storage API.
// Only the operations a CSI driver needs are implemented.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultBaseURL  = "https://api.e2enetworks.com/myaccount/api/v1"
	defaultLocation = "Delhi"
)

// Client talks to the E2E Networks MyAccount API.
type Client struct {
	BaseURL    string
	APIKey     string
	AuthToken  string
	Location   string
	ProjectID  string
	HTTP       *http.Client
}

// New constructs a client. ProjectID may be set later per-call if multi-project.
func New(apiKey, authToken, projectID string) *Client {
	return &Client{
		BaseURL:   defaultBaseURL,
		APIKey:    apiKey,
		AuthToken: authToken,
		Location:  defaultLocation,
		ProjectID: projectID,
		HTTP:      &http.Client{Timeout: 30 * time.Second},
	}
}

// envelope mirrors the JSON wrapper E2E uses for every response.
type envelope struct {
	Code    int             `json:"code"`
	Data    json.RawMessage `json:"data"`
	Errors  json.RawMessage `json:"errors"`
	Message string          `json:"message"`
}

// Volume is the subset of the volume payload the driver needs.
type Volume struct {
	BlockID  int    `json:"block_id"`
	Name     string `json:"name"`
	SizeMiB  int    `json:"size"`        // bytes? MiB? — observed 95368 for 100 GB → MiB
	Status   string `json:"status"`      // "Available", "Attached", ...
	VMDetail struct {
		NodeID int    `json:"node_id"` // matches /nodes/ id
		VMID   int    `json:"vm_id"`   // matches block-storage attach id
		Name   string `json:"vm_name"`
	} `json:"vm_detail"`
	Template struct {
		DevPrefix string `json:"DEV_PREFIX"` // e.g. "vd"
	} `json:"template"`
}

// EligibleVM is one entry in the GET /vm/attach/ response.
type EligibleVM struct {
	Name      string `json:"name"`
	VMID      int    `json:"vm_id"`
	PublicIP  string `json:"ip_address_public"`
	PrivateIP string `json:"ip_address_private"`
}

// Node is the subset of the /nodes/ response the driver needs.
type Node struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	PublicIP  string `json:"public_ip_address"`
	PrivateIP string `json:"private_ip_address"`
}

// ----- request plumbing -------------------------------------------------------

func (c *Client) do(ctx context.Context, method, path string, body interface{}) ([]byte, error) {
	u, err := url.Parse(c.BaseURL + path)
	if err != nil {
		return nil, fmt.Errorf("bad URL: %w", err)
	}
	q := u.Query()
	q.Set("apikey", c.APIKey)
	q.Set("location", c.Location)
	if c.ProjectID != "" {
		q.Set("project_id", c.ProjectID)
	}
	u.RawQuery = q.Encode()

	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal: %w", err)
		}
		rdr = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, u.String(), rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.AuthToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "csi-e2enetworks/0.1")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	if os.Getenv("E2E_DEBUG") != "" {
		dump, _ := httputil.DumpRequestOut(req, true)
		fmt.Fprintf(os.Stderr, "\n--- request ---\n%s\n", dump)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		// not our envelope (likely an HTML error page) — surface what we can
		return nil, fmt.Errorf("unexpected response (HTTP %d, %s %s, ct=%q, hdrs=%v): %s",
			resp.StatusCode, method, u.Path, resp.Header.Get("Content-Type"),
			resp.Header, snip(raw, 1500))
	}
	if env.Code >= 200 && env.Code < 300 {
		return env.Data, nil
	}
	// Build a useful error string. E2E's error shape varies: some endpoints
	// populate `errors` (e.g. validation), others (notably the OpenNebula-
	// backed paths like /vm/upgrade/) leave errors={} and stuff the real
	// message into `data` as a string while `message` is just the HTTP-class
	// phrase. Include all three when they carry info so the caller's
	// classifiers (isVMHotPlugErr, isAlreadyDetachedErr, ...) can match.
	parts := []string{}
	if s := strings.TrimSpace(string(env.Errors)); s != "" && s != "{}" && s != "null" {
		parts = append(parts, s)
	}
	if env.Message != "" {
		parts = append(parts, env.Message)
	}
	if s := strings.TrimSpace(string(env.Data)); s != "" && s != "null" && s != "{}" {
		parts = append(parts, "data="+s)
	}
	return nil, fmt.Errorf("e2e api: HTTP %d (api code %d): %s", resp.StatusCode, env.Code, strings.Join(parts, " | "))
}

func snip(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}

// ----- public methods ---------------------------------------------------------

// CreateVolume provisions a new block volume. SizeGB must be ≥ 100.
// IOPS = 1500 for 100 GB tier, 3000 for 200 GB, etc.
func (c *Client) CreateVolume(ctx context.Context, name string, sizeGB, iops int) (*Volume, error) {
	body := map[string]interface{}{"name": name, "size": sizeGB, "iops": iops}
	raw, err := c.do(ctx, http.MethodPost, "/block_storage/", body)
	if err != nil {
		return nil, err
	}
	// create response is small: {id, image_name, ...}
	var created struct {
		ID   int    `json:"id"`
		Name string `json:"image_name"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		return nil, fmt.Errorf("decode create: %w", err)
	}
	// fetch full record
	return c.GetVolume(ctx, created.ID)
}

// GetVolume returns the full volume record.
func (c *Client) GetVolume(ctx context.Context, id int) (*Volume, error) {
	raw, err := c.do(ctx, http.MethodGet, "/block_storage/"+strconv.Itoa(id)+"/", nil)
	if err != nil {
		return nil, err
	}
	var v Volume
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("decode volume: %w", err)
	}
	return &v, nil
}

// ListVolumes returns all volumes in the project.
func (c *Client) ListVolumes(ctx context.Context) ([]Volume, error) {
	raw, err := c.do(ctx, http.MethodGet, "/block_storage/", nil)
	if err != nil {
		return nil, err
	}
	var vs []Volume
	if err := json.Unmarshal(raw, &vs); err != nil {
		return nil, fmt.Errorf("decode volumes: %w", err)
	}
	return vs, nil
}

// DeleteVolume removes a volume. Volume must be detached (status=Available).
func (c *Client) DeleteVolume(ctx context.Context, id int) error {
	_, err := c.do(ctx, http.MethodDelete, "/block_storage/"+strconv.Itoa(id)+"/", nil)
	return err
}

// EligibleVMs returns the list of VMs the volume can attach to.
// The vm_id values returned here are what /vm/attach/ and /vm/detach/ expect.
func (c *Client) EligibleVMs(ctx context.Context, volumeID int) ([]EligibleVM, error) {
	raw, err := c.do(ctx, http.MethodGet, "/block_storage/"+strconv.Itoa(volumeID)+"/vm/attach/", nil)
	if err != nil {
		return nil, err
	}
	var out []EligibleVM
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode eligible: %w", err)
	}
	return out, nil
}

// AttachVolume attaches the volume to the VM. PUT, not POST.
//
// CreateVolume returns immediately with status=Creating; if a caller (e.g.
// CSI's ControllerPublishVolume right after CreateVolume) issues attach
// before the volume is Available, the API replies HTTP 500. Wait for the
// volume to become Available before issuing the PUT.
//
// Workaround #2: E2E's API occasionally returns an HTML rate-limit page
// even when the PUT went through. Swallow non-JSON responses there and
// verify the actual outcome by polling status.
func (c *Client) AttachVolume(ctx context.Context, volumeID, vmID int) error {
	// Wait until the volume is Available (or already Attached to this VM,
	// in which case attach is a no-op). Cap at 60s — Creating typically
	// resolves in 10-15s; longer than that means something is wrong.
	wctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	for {
		v, err := c.GetVolume(wctx, volumeID)
		if err != nil {
			return fmt.Errorf("attach precheck: %w", err)
		}
		if v.Status == "Attached" && v.VMDetail.VMID == vmID {
			return nil // already on the target VM
		}
		if v.Status == "Attached" && v.VMDetail.VMID != vmID {
			return fmt.Errorf("volume %d already attached to vm_id=%d, want %d", volumeID, v.VMDetail.VMID, vmID)
		}
		if v.Status == "Available" {
			break
		}
		select {
		case <-wctx.Done():
			return fmt.Errorf("attach precheck: timeout waiting for volume %d to be Available (last=%s)", volumeID, v.Status)
		case <-time.After(1 * time.Second):
		}
	}

	_, err := c.do(ctx, http.MethodPut,
		"/block_storage/"+strconv.Itoa(volumeID)+"/vm/attach/",
		map[string]int{"vm_id": vmID})
	if err != nil && !isHTMLLimitErr(err) {
		return err
	}
	wctx2, cancel2 := context.WithTimeout(ctx, 30*time.Second)
	defer cancel2()
	v, err := c.WaitForStatus(wctx2, volumeID, "Attached", 1*time.Second)
	if err != nil {
		return fmt.Errorf("attach verify: %w", err)
	}
	if v.VMDetail.VMID != vmID {
		return fmt.Errorf("attach landed on vm_id=%d, want %d", v.VMDetail.VMID, vmID)
	}
	return nil
}

// DetachVolume detaches the volume from a specific VM.
//
// Semantics CSI cares about: "is the volume no longer attached to *this* VM?"
// That's true when (a) the volume is Available, or (b) the volume happens to
// be attached to some other VM (e.g. CSI raced with a cross-node move). Both
// outcomes resolve the per-(volume,node) VolumeAttachment object that CSI is
// reconciling, so we treat both as success.
//
// Idempotency:
//   - If the volume is already not on this VM, short-circuit before the PUT.
//   - If the PUT returns 412 "Disk is not attached" or an HTML rate-limit
//     page, swallow it and verify the desired end state by polling.
func (c *Client) DetachVolume(ctx context.Context, volumeID, vmID int) error {
	detachedFromUs := func(v *Volume) bool {
		if v.Status == "Available" {
			return true
		}
		if v.Status == "Attached" && v.VMDetail.VMID != vmID {
			return true
		}
		return false
	}

	v, err := c.GetVolume(ctx, volumeID)
	if err != nil {
		return fmt.Errorf("detach precheck: %w", err)
	}
	if detachedFromUs(v) {
		return nil
	}

	_, err = c.do(ctx, http.MethodPut,
		"/block_storage/"+strconv.Itoa(volumeID)+"/vm/detach/",
		map[string]int{"vm_id": vmID})
	if err != nil && !isHTMLLimitErr(err) && !isAlreadyDetachedErr(err) {
		return err
	}

	wctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	for {
		v, err := c.GetVolume(wctx, volumeID)
		if err != nil {
			return fmt.Errorf("detach verify: %w", err)
		}
		if detachedFromUs(v) {
			return nil
		}
		select {
		case <-wctx.Done():
			return fmt.Errorf("detach verify: timeout waiting for volume %d to release vm_id=%d (last status=%s, on vm_id=%d)",
				volumeID, vmID, v.Status, v.VMDetail.VMID)
		case <-time.After(1 * time.Second):
		}
	}
}

// UpgradeVolume resizes an attached block volume to newSizeGB. E2E exposes
// this as PUT /block_storage/{id}/vm/upgrade/ with payload
// {vm_id, block_storage_size, name}. The endpoint requires the volume to be
// attached to vmID — there is no offline-resize path. name must be the
// volume's current name (per the support team's API contract).
//
// Behaviour notes (probed against the live API in May 2026):
//   - Returns 200 immediately; the volume stays status=Attached throughout.
//   - The `size` field (MiB) updates ~15-20s after the call. The `bs_size`
//     field (decimal GB) updates immediately to the requested value.
//   - HTML rate-limit page is swallowed exactly like attach/detach do.
//   - Right after a fresh attach (typically within ~30-60s) the underlying
//     OpenNebula VM is still in HOTPLUG state and the API returns 500 with
//     `wrong state HOTPLUG`. We retry that transient case for ~90s; once the
//     VM transitions to RUNNING the resize succeeds. In real CSI use the
//     volume has been attached for minutes-to-days before resize, so this
//     retry only matters for create→attach→resize sequences in tests.
func (c *Client) UpgradeVolume(ctx context.Context, volumeID, vmID, newSizeGB int, name string) error {
	body := map[string]interface{}{
		"vm_id":              vmID,
		"block_storage_size": newSizeGB,
		"name":               name,
	}
	path := "/block_storage/" + strconv.Itoa(volumeID) + "/vm/upgrade/"

	deadline := time.Now().Add(90 * time.Second)
	for {
		_, err := c.do(ctx, http.MethodPut, path, body)
		if err == nil || isHTMLLimitErr(err) {
			return nil
		}
		if !isVMHotPlugErr(err) || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// isVMHotPlugErr matches the OpenNebula error returned while the underlying
// VM is still in HOTPLUG state (i.e. an attach hasn't fully transitioned to
// RUNNING). Transient — clears in tens of seconds.
func isVMHotPlugErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "wrong state hotplug") ||
		strings.Contains(s, "diskresize") && strings.Contains(s, "hotplug")
}

func isHTMLLimitErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "text/html")
}

// isAlreadyDetachedErr matches the API's 412 response when a detach is
// issued for a volume/VM pair that isn't actually attached. CSI attaches
// retry detach idempotently, so we treat this as success.
func isAlreadyDetachedErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "disk is not attached") ||
		strings.Contains(s, "not attached to vm")
}

// WaitForStatus polls GetVolume until the status matches or the context times out.
func (c *Client) WaitForStatus(ctx context.Context, id int, want string, every time.Duration) (*Volume, error) {
	for {
		v, err := c.GetVolume(ctx, id)
		if err != nil {
			return nil, err
		}
		if v.Status == want {
			return v, nil
		}
		select {
		case <-ctx.Done():
			return v, errors.New("timeout waiting for status=" + want + " (last=" + v.Status + ")")
		case <-time.After(every):
		}
	}
}

// ListNodes returns the project's compute nodes.
func (c *Client) ListNodes(ctx context.Context) ([]Node, error) {
	raw, err := c.do(ctx, http.MethodGet, "/nodes/", nil)
	if err != nil {
		return nil, err
	}
	var ns []Node
	if err := json.Unmarshal(raw, &ns); err != nil {
		return nil, fmt.Errorf("decode nodes: %w", err)
	}
	return ns, nil
}

// VMIDByIP looks up the block-storage vm_id for a node identified by its IP
// (private or public). Pass any volume id (or 0 to allocate one); the API
// returns the same eligible-VM list regardless of which volume you query.
func (c *Client) VMIDByIP(ctx context.Context, anyVolumeID int, ip string) (int, error) {
	vms, err := c.EligibleVMs(ctx, anyVolumeID)
	if err != nil {
		return 0, err
	}
	for _, v := range vms {
		if v.PrivateIP == ip || v.PublicIP == ip {
			return v.VMID, nil
		}
	}
	return 0, fmt.Errorf("no VM matching IP %q in project (known: %d)", ip, len(vms))
}
