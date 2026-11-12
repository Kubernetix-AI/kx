package cmd

import (
	"strings"
	"testing"
)

func TestRollback_AsksOnATerminal(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	h.api.deployments[appShopID][1].ImageURL = "registry.kubernetix.ai/acme-corp/shop-api@sha256:" + strings.Repeat("14", 32)
	h.api.deployments[appShopID][2].ImageURL = "registry.kubernetix.ai/acme-corp/shop-api@sha256:" + strings.Repeat("13", 32)
	h.tty, h.stdin = true, "y\n"
	h.expect(h.run("rollback"), 0)
	if !strings.Contains(h.err.String(), "Roll shop-api back from v14 to v13? [y/N] ") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
	if len(h.api.rollbacks) != 1 || h.api.rollbacks[0] != "dep-13" {
		t.Fatalf("rollbacks = %v", h.api.rollbacks)
	}
	if h.out.String() != "https://"+appShopID+"--acme-corp.apps.example.test\n" {
		t.Fatalf("stdout = %q", h.out.String())
	}
	if !strings.Contains(h.norm(h.err.String()), "✓ Started   v16 with the image of v13\n✓ Live      v16  queued → starting → live\n") {
		t.Fatalf("stderr:\n%s", h.err.String())
	}
}

func TestRollback_No(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	h.api.deployments[appShopID][2].ImageURL = "other"
	h.tty, h.stdin = true, "\n"
	h.expect(h.run("rollback"), 0)
	if len(h.api.rollbacks) != 0 || !strings.Contains(h.err.String(), "Nothing changed.") {
		t.Fatalf("rollbacks %v\n%s", h.api.rollbacks, h.err.String())
	}
}

func TestRollback_NeedsYesWithoutTerminal(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	h.expect(h.run("rollback"), 2)
	if !strings.Contains(h.err.String(), "kx rollback needs --yes when it can't ask first.\nRun: kx rollback --yes") || len(h.api.got()) != 0 {
		t.Fatalf("stderr %q requests %v", h.err.String(), h.api.got())
	}
	h.api.deployments[appShopID][2].ImageURL = "other"
	h.expect(h.run("rollback", "--to", "v13", "--yes"), 0)
	if len(h.api.rollbacks) != 1 || h.api.rollbacks[0] != "dep-13" {
		t.Fatalf("rollbacks = %v", h.api.rollbacks)
	}
}

func TestRollback_Errors(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	h.expect(h.run("rollback", "--to", "soon", "--yes"), 2)
	if !strings.Contains(h.err.String(), "kx rollback --to takes a version like v13.") {
		t.Fatalf("stderr %q", h.err.String())
	}
	h.expect(h.run("rollback", "--to", "v14", "--yes"), 2)
	if !strings.Contains(h.err.String(), "shop-api v14 is already live.") {
		t.Fatalf("stderr %q", h.err.String())
	}
	h.expect(h.run("rollback", "--to", "v15", "--yes"), 2)
	if !strings.Contains(h.err.String(), "shop-api v15 never went live, so kx can't roll back to it.") {
		t.Fatalf("stderr %q", h.err.String())
	}
	h.expect(h.run("rollback", "--yes"), 4) // v13 has the same (empty) image as v14
	if !strings.Contains(h.err.String(), "shop-api has no earlier live version with a different image to roll back to.") {
		t.Fatalf("stderr %q", h.err.String())
	}
}

func TestRollback_UnconfirmedWhenKubernetixDoesNotAnswer(t *testing.T) {
	h := newHarness(t)
	h.loginAs("acme-corp", tokCorp, &dec11)
	h.link("shop-api", appShopID, "acme-corp")
	h.api.deployments[appShopID][2].ImageURL = "other"
	h.api.rollbackErr = 503
	h.expect(h.run("rollback", "--yes"), 1)
	if len(h.api.rollbacks) != 1 {
		t.Fatalf("sent the rollback %d times, want once", len(h.api.rollbacks))
	}
	want := "kx couldn't confirm the rollback of shop-api to v13 because Kubernetix didn't answer, so it may or may not have started.\nCheck: kx status\n"
	if !strings.HasSuffix(h.err.String(), want) || h.out.String() != "" {
		t.Fatalf("stderr = %q stdout = %q", h.err.String(), h.out.String())
	}
}
