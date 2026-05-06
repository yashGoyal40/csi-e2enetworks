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
	errStr := string(env.Errors)
	if errStr == "" || errStr == "{}" {
		errStr = env.Message
	}
	return nil, fmt.Errorf("e2e api: HTTP %d (api code %d): %s", resp.StatusCode, env.Code, errStr)
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
// Workaround: E2E's API occasionally returns an HTML rate-limit page even
// when the operation went through. We swallow non-JSON responses on the PUT,
// then verify the actual outcome by polling status.
func (c *Client) AttachVolume(ctx context.Context, volumeID, vmID int) error {
	_, err := c.do(ctx, http.MethodPut,
		"/block_storage/"+strconv.Itoa(volumeID)+"/vm/attach/",
		map[string]int{"vm_id": vmID})
	if err != nil && !isHTMLLimitErr(err) {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	v, err := c.WaitForStatus(wctx, volumeID, "Attached", 2*time.Second)
	if err != nil {
		return fmt.Errorf("attach verify: %w", err)
	}
	if v.VMDetail.VMID != vmID {
		return fmt.Errorf("attach landed on vm_id=%d, want %d", v.VMDetail.VMID, vmID)
	}
	return nil
}

// DetachVolume kicks off detach. Async — status flips to Available in ~10s.
// We swallow HTML responses (same buggy API behavior as Attach) and verify
// success by waiting for status=Available.
func (c *Client) DetachVolume(ctx context.Context, volumeID, vmID int) error {
	_, err := c.do(ctx, http.MethodPut,
		"/block_storage/"+strconv.Itoa(volumeID)+"/vm/detach/",
		map[string]int{"vm_id": vmID})
	if err != nil && !isHTMLLimitErr(err) {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if _, err := c.WaitForStatus(wctx, volumeID, "Available", 3*time.Second); err != nil {
		return fmt.Errorf("detach verify: %w", err)
	}
	return nil
}

func isHTMLLimitErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "text/html")
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
