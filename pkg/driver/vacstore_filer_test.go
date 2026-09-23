package driver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/pb"
)

// newVacServer records which volume keys were requested, and with which method.
type vacServer struct {
	*httptest.Server
	mu       chan struct{}
	requests []string
	body     string
	status   int
}

func newVacServer(t *testing.T, status int, body string) *vacServer {
	t.Helper()
	s := &vacServer{status: status, body: body, mu: make(chan struct{}, 1)}
	s.mu <- struct{}{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-s.mu
		s.requests = append(s.requests, r.Method+" "+r.URL.Path)
		s.mu <- struct{}{}
		// The filer answers writes and deletes with their own codes; only
		// GET returns the configured status/body the read tests care about.
		switch r.Method {
		case http.MethodPut, http.MethodPost:
			w.WriteHeader(http.StatusCreated)
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(s.status)
			if s.body != "" {
				_, _ = w.Write([]byte(s.body))
			}
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *vacServer) seen() []string {
	<-s.mu
	defer func() { s.mu <- struct{}{} }()
	out := make([]string, len(s.requests))
	copy(out, s.requests)
	return out
}

func (s *vacServer) address() string { return strings.TrimPrefix(s.URL, "http://") }

func TestVacFilersForVolumeUsesTheEncodedFiler(t *testing.T) {
	driver := &SeaweedFsDriver{filers: pb.ServerAddresses("default-filer:8888").ToAddresses()}

	got := vacFilersForVolume(driver, "filer://tenant-filer:8888/buckets/pvc-1")
	if len(got) != 1 || string(got[0]) != "tenant-filer:8888" {
		t.Errorf("expected the volume's own filer, got %v", got)
	}

	// A legacy bare ID encodes no filer, so the driver default is all there is.
	got = vacFilersForVolume(driver, "/buckets/pvc-legacy")
	if len(got) != 1 || string(got[0]) != "default-filer:8888" {
		t.Errorf("expected the driver default for a legacy ID, got %v", got)
	}
}

// The outage: the read went to the driver's default filer, so a tenant
// volume's attributes were looked up on the wrong filer entirely.
func TestLoadPersistedVolumeAttributesReadsTheVolumesOwnFiler(t *testing.T) {
	tenant := newVacServer(t, http.StatusOK, `{"parameters":{"concurrentReaders":"64"}}`)
	def := newVacServer(t, http.StatusOK, `{"parameters":{"concurrentReaders":"999"}}`)

	ns := &NodeServer{Driver: &SeaweedFsDriver{filers: pb.ServerAddresses(def.address()).ToAddresses()}}
	volumeID := "filer://" + tenant.address() + "/buckets/pvc-1"

	params, err := ns.loadPersistedVolumeAttributes(context.Background(), volumeID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if params["concurrentReaders"] != "64" {
		t.Errorf("read the wrong filer's entry: %v", params)
	}
	if len(tenant.seen()) != 1 {
		t.Errorf("expected exactly one request to the volume's filer, got %v", tenant.seen())
	}
	if n := len(def.seen()); n != 0 {
		t.Errorf("the default filer must not be consulted, got %v", def.seen())
	}
}

// An optional feature's metadata lookup must not be able to block mounting.
func TestLoadPersistedVolumeAttributesDegradesWhenStoreUnreachable(t *testing.T) {
	// Bind and immediately close, so the address is routable but refuses.
	dead := newVacServer(t, http.StatusOK, "")
	address := dead.address()
	dead.Close()

	ns := &NodeServer{Driver: &SeaweedFsDriver{filers: pb.ServerAddresses("default-filer:8888").ToAddresses()}}

	params, err := ns.loadPersistedVolumeAttributes(context.Background(), "filer://"+address+"/buckets/pvc-1")
	if err != nil {
		t.Fatalf("an unreachable store must not fail the read: %v", err)
	}
	if params != nil {
		t.Errorf("expected no parameters, got %v", params)
	}
}

// ...and the mount itself must go through.
func TestStageNewVolumeSucceedsWhenVacStoreUnreachable(t *testing.T) {
	dead := newVacServer(t, http.StatusOK, "")
	address := dead.address()
	dead.Close()

	ns := newTestNodeServer(t, &fakeMounter{})
	ns.vacLoader = nil // exercise the real store
	ns.Driver.filers = pb.ServerAddresses("default-filer:8888").ToAddresses()

	stagingPath := filepath.Join(t.TempDir(), "staging")
	if _, err := ns.stageNewVolume("filer://"+address+"/buckets/pvc-1", stagingPath, map[string]string{}, false); err != nil {
		t.Fatalf("staging must survive an unreachable VAC store: %v", err)
	}
}

// A stored document that is present and wrong is a real inconsistency, not an
// availability problem, and must still surface.
func TestLoadPersistedVolumeAttributesFailsOnCorruptEntry(t *testing.T) {
	broken := newVacServer(t, http.StatusOK, "{not json")

	ns := &NodeServer{Driver: &SeaweedFsDriver{filers: pb.ServerAddresses("default-filer:8888").ToAddresses()}}

	_, err := ns.loadPersistedVolumeAttributes(context.Background(), "filer://"+broken.address()+"/buckets/pvc-1")
	if err == nil {
		t.Fatal("a corrupt entry must not be silently ignored")
	}
	var corrupt *errCorruptVacEntry
	if !errors.As(err, &corrupt) {
		t.Errorf("expected errCorruptVacEntry, got %T: %v", err, err)
	}
}

// ControllerModifyVolume writes the entry keyed by the raw volume ID, so
// DeleteVolume must delete that same key. resolveVolume reassigns volumeId to
// the decoded path partway through DeleteVolume, and keying off that produced
// a base64 path that was never written — a silent orphan per modified volume,
// swallowed by the warning-only error handling.
func TestDeleteVolumeDeletesTheKeyModifyWrote(t *testing.T) {
	filerServer := newVacServer(t, http.StatusNotFound, "")
	address := filerServer.address()
	rawVolumeID := "filer://" + address + "/buckets/pvc-key"

	driver := &SeaweedFsDriver{filers: pb.ServerAddresses(address).ToAddresses()}
	cs := &ControllerServer{Driver: driver}

	writeStore, err := cs.storeForVolume(rawVolumeID)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := writeStore.Write(context.Background(), rawVolumeID, map[string]string{"concurrentReaders": "64"}); err != nil {
		t.Fatalf("write: %v", err)
	}

	deleteStore, err := cs.storeForVolume(rawVolumeID)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if err := deleteStore.Delete(context.Background(), rawVolumeID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	var wrote, deleted string
	for _, r := range filerServer.seen() {
		method, path, _ := strings.Cut(r, " ")
		switch method {
		case http.MethodPut, http.MethodPost:
			wrote = path
		case http.MethodDelete:
			deleted = path
		}
	}
	if wrote == "" || deleted == "" {
		t.Fatalf("expected both a write and a delete, saw %v", filerServer.seen())
	}
	if wrote != deleted {
		t.Errorf("delete targeted a different key than write:\n  wrote   %s\n  deleted %s", wrote, deleted)
	}
	// And the key must be the encoding of the raw ID, not of the decoded path.
	if want := vacPath(rawVolumeID); deleted != want {
		t.Errorf("deleted key = %s, want %s (encoding of the raw volume ID)", deleted, want)
	}
}
