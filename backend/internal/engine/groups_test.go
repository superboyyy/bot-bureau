package engine

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestGroupCRUDAndMembershipPersistence(t *testing.T) {
	_, bus, sched := newTestWorker(t, "a", nil)
	addTestBot(t, bus, sched, "c")
	path := filepath.Join(t.TempDir(), "groups.json")
	bus.LoadGroups(path, "", "Team", "#123456")

	g, err := bus.CreateGroup("  Work room ", "#abcdef", []string{"a", "a", "c", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if g.Title != "Work room" || !reflect.DeepEqual(g.Members, []string{"a", "c"}) {
		t.Fatalf("created group = %#v", g)
	}
	title, avatar, members := " Updated ", "#010203", []string{"c", "c"}
	if err := bus.UpdateGroup(g.ID, &title, &avatar, &members); err != nil {
		t.Fatal(err)
	}
	groups := bus.Groups()
	var got Group
	for _, item := range groups {
		if item.ID == g.ID {
			got = item
		}
	}
	if got.Title != "Updated" || got.Avatar != avatar || !reflect.DeepEqual(got.Members, []string{"c"}) {
		t.Fatalf("updated group = %#v", got)
	}
	if err := bus.UpdateGroup("missing", nil, nil, nil); err == nil {
		t.Fatal("updating a missing group should fail")
	}
	if err := bus.DeleteGroup("group"); err == nil || bus.DeleteGroup("missing") == nil {
		t.Fatal("deleting protected or missing groups should fail")
	}
	if err := bus.DeleteGroup(g.ID); err != nil {
		t.Fatal(err)
	}

	// Create one group again and reload it through a fresh Bus to verify the on-disk format.
	g, err = bus.CreateGroup("Persisted", "", []string{"a", "c"})
	if err != nil {
		t.Fatal(err)
	}
	_, freshBus, freshSched := newTestWorker(t, "a", nil)
	addTestBot(t, freshBus, freshSched, "c")
	freshBus.LoadGroups(path, "", "Team", "#123456")
	if !freshBus.IsGroupMemberOf(g.ID, "a") || !freshBus.IsGroupMemberOf(g.ID, "c") {
		t.Fatalf("persisted members missing: %#v", freshBus.GroupMembersOf(g.ID))
	}

	// Removing a worker removes it from every group, leaving no dangling membership.
	bus.Unregister("c")
	if bus.IsGroupMemberOf(g.ID, "c") {
		t.Fatal("unregistered bot remained in group")
	}
}

func TestLegacyGroupFileMigratesAndFiltersBots(t *testing.T) {
	_, bus, sched := newTestWorker(t, "a", nil)
	addTestBot(t, bus, sched, "c")
	path := filepath.Join(t.TempDir(), "groups.json")
	legacy := filepath.Join(t.TempDir(), "group.json")
	if err := writeJSONFile(legacy, `{"members":["c","missing","c"]}`); err != nil {
		t.Fatal(err)
	}
	bus.LoadGroups(path, legacy, "Legacy team", "")
	if got, want := bus.GroupMembers(), []string{"c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy members = %#v, want %#v", got, want)
	}
	if bus.Groups()[0].Title != "Legacy team" {
		t.Fatalf("legacy group metadata = %#v", bus.Groups()[0])
	}
}

func writeJSONFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o644)
}
