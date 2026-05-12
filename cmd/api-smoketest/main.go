// api-smoketest exercises the E2E client end-to-end:
//   create → wait Available → attach → detach → wait Available → delete
//
// Build & run:
//   go run ./cmd/api-smoketest -nodeip <node-private-ip>
//
// Reads E2E_API_KEY, E2E_AUTH_TOKEN, E2E_PROJECT_ID, E2E_LOCATION from env.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/yashgoyal40/csi-e2enetworks/pkg/e2e"
)

func main() {
	nodeIP := flag.String("nodeip", "", "private IP of the node to attach to")
	keep := flag.Bool("keep", false, "skip detach+delete (leave the volume around)")
	resizeGB := flag.Int("resize", 0, "if >0, after attach call /vm/upgrade/ to grow the volume to this size (GB) and poll for it to settle")
	flag.Parse()

	apiKey := os.Getenv("E2E_API_KEY")
	auth := os.Getenv("E2E_AUTH_TOKEN")
	proj := os.Getenv("E2E_PROJECT_ID")
	if apiKey == "" || auth == "" || proj == "" {
		die("E2E_API_KEY / E2E_AUTH_TOKEN / E2E_PROJECT_ID must be set")
	}
	if *nodeIP == "" {
		die("--nodeip is required (private IP of any node in the project)")
	}
	c := e2e.New(apiKey, auth, proj)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	step("create 100GB volume...")
	v, err := c.CreateVolume(ctx, fmt.Sprintf("smoketest-%d", time.Now().Unix()), 100, 1500)
	if err != nil {
		die("create: %v", err)
	}
	fmt.Printf("  ok: id=%d name=%q status=%s\n", v.BlockID, v.Name, v.Status)

	step("wait for status=Available before attach...")
	wctx0, wc0 := context.WithTimeout(ctx, 90*time.Second)
	v, err = c.WaitForStatus(wctx0, v.BlockID, "Available", 3*time.Second)
	wc0()
	if err != nil {
		die("wait Available: %v", err)
	}
	fmt.Printf("  ok: status=%s\n", v.Status)

	step("look up vm_id by ip %s...", *nodeIP)
	vmID, err := c.VMIDByIP(ctx, v.BlockID, *nodeIP)
	if err != nil {
		die("vm lookup: %v", err)
	}
	fmt.Printf("  ok: vm_id=%d\n", vmID)

	step("attach volume %d to vm %d...", v.BlockID, vmID)
	if err := c.AttachVolume(ctx, v.BlockID, vmID); err != nil {
		die("attach: %v", err)
	}
	v2, err := c.GetVolume(ctx, v.BlockID)
	if err != nil {
		die("get post-attach: %v", err)
	}
	fmt.Printf("  ok: status=%s vm_detail=%+v\n", v2.Status, v2.VMDetail)

	if *resizeGB > 0 {
		step("upgrade volume %d to %d GB (vm_id=%d, name=%q)...", v.BlockID, *resizeGB, vmID, v2.Name)
		if err := c.UpgradeVolume(ctx, v.BlockID, vmID, *resizeGB, v2.Name); err != nil {
			die("upgrade: %v", err)
		}
		step("poll for size to settle...")
		wantMiB := *resizeGB * 1024
		deadline := time.Now().Add(90 * time.Second)
		for time.Now().Before(deadline) {
			got, err := c.GetVolume(ctx, v.BlockID)
			if err != nil {
				die("post-upgrade get: %v", err)
			}
			fmt.Printf("  size=%d MiB status=%s\n", got.SizeMiB, got.Status)
			if got.SizeMiB >= wantMiB {
				break
			}
			time.Sleep(3 * time.Second)
		}
	}

	if *keep {
		step("--keep set, leaving volume id=%d attached.", v.BlockID)
		return
	}

	step("sleeping 5s before detach (real K8s lifecycles never hit this rate-limit)...")
	time.Sleep(5 * time.Second)
	step("detach...")
	if err := c.DetachVolume(ctx, v.BlockID, vmID); err != nil {
		die("detach: %v", err)
	}
	step("wait for status=Available...")
	wctx, wcancel := context.WithTimeout(ctx, 90*time.Second)
	v3, err := c.WaitForStatus(wctx, v.BlockID, "Available", 3*time.Second)
	wcancel()
	if err != nil {
		die("wait: %v", err)
	}
	fmt.Printf("  ok: status=%s\n", v3.Status)

	step("delete...")
	if err := c.DeleteVolume(ctx, v.BlockID); err != nil {
		die("delete: %v", err)
	}
	fmt.Println("  ok")

	step("done.")
}

func step(format string, args ...interface{}) {
	fmt.Printf(">> "+format+"\n", args...)
}

func die(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "ERR "+format+"\n", args...)
	os.Exit(1)
}
