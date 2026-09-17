package store

import (
	"context"
	"errors"
	"testing"
)

func TestKeyAuthBindingsReplaceAndStayPerKey(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	defer st.Close()
	first := newTestKey(t, ctx, st, PluginKeySpec{Label: "first"})
	second := newTestKey(t, ctx, st, PluginKeySpec{Kid: "test-key-2", Label: "second"})

	if err := st.ReplaceKeyAuthBindings(ctx, first.ID, []KeyAuthBinding{
		{Provider: " Codex ", AuthID: " account-a "},
		{Provider: "codex", AuthID: "account-a"},
		{Provider: "codex", AuthID: "account-b"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceKeyAuthBindings(ctx, second.ID, []KeyAuthBinding{{Provider: "codex", AuthID: "account-b"}}); err != nil {
		t.Fatal(err)
	}
	got, err := st.ListKeyAuthBindings(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Provider != "codex" || got[0].AuthID != "account-a" || got[1].AuthID != "account-b" {
		t.Fatalf("first bindings = %#v", got)
	}
	other, err := st.ListKeyAuthBindings(ctx, second.ID)
	if err != nil || len(other) != 1 || other[0].AuthID != "account-b" {
		t.Fatalf("second bindings = %#v err=%v", other, err)
	}

	if err := st.ReplaceKeyAuthBindings(ctx, first.ID, []KeyAuthBinding{{Provider: "xai", AuthID: "account-c"}}); err != nil {
		t.Fatal(err)
	}
	if got, err = st.ListKeyAuthBindings(ctx, first.ID); err != nil || len(got) != 1 || got[0].Provider != "xai" {
		t.Fatalf("replaced bindings = %#v err=%v", got, err)
	}
	if err := st.ReplaceKeyAuthBindings(ctx, first.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got, err = st.ListKeyAuthBindings(ctx, first.ID); err != nil || len(got) != 0 {
		t.Fatalf("cleared bindings = %#v err=%v", got, err)
	}
}

func TestReplaceKeyAuthBindingsRejectsIncompleteRows(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	defer st.Close()
	key := newTestKey(t, ctx, st, PluginKeySpec{Label: "bind"})
	if err := st.ReplaceKeyAuthBindings(ctx, key.ID, []KeyAuthBinding{{Provider: "codex"}}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("missing auth id error = %v", err)
	}
	if err := st.ReplaceKeyAuthBindings(ctx, key.ID, []KeyAuthBinding{{AuthID: "account-a"}}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("missing provider error = %v", err)
	}
}

func TestListKeyAuthBindingsByKeyIDsGroupsAndIgnoresUnknownKeys(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	defer st.Close()
	first := newTestKey(t, ctx, st, PluginKeySpec{Label: "first"})
	second := newTestKey(t, ctx, st, PluginKeySpec{Kid: "test-key-2", Label: "second"})
	third := newTestKey(t, ctx, st, PluginKeySpec{Kid: "test-key-3", Label: "third"})

	// Keys A and B overlap on account-b, which must stay scoped per key.
	if err := st.ReplaceKeyAuthBindings(ctx, first.ID, []KeyAuthBinding{
		{Provider: "codex", AuthID: "account-a"},
		{Provider: "codex", AuthID: "account-b"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceKeyAuthBindings(ctx, second.ID, []KeyAuthBinding{
		{Provider: "codex", AuthID: "account-b"},
		{Provider: "codex", AuthID: "account-c"},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := st.ListKeyAuthBindingsByKeyIDs(ctx, []string{first.ID, second.ID, first.ID, third.ID, ""})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("grouped bindings = %#v", got)
	}
	if list := got[first.ID]; len(list) != 2 || list[0].AuthID != "account-a" || list[1].AuthID != "account-b" {
		t.Fatalf("first bindings = %#v", list)
	}
	if list := got[second.ID]; len(list) != 2 || list[0].AuthID != "account-b" || list[1].AuthID != "account-c" {
		t.Fatalf("second bindings = %#v", list)
	}
	if _, ok := got[third.ID]; ok {
		t.Fatalf("unbound key must be absent, got %#v", got[third.ID])
	}
	if empty, err := st.ListKeyAuthBindingsByKeyIDs(ctx, nil); err != nil || len(empty) != 0 {
		t.Fatalf("empty request = %#v err=%v", empty, err)
	}
}

func TestDeletePluginKeyDropsAuthBindings(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	defer st.Close()
	key := newTestKey(t, ctx, st, PluginKeySpec{Label: "bind"})
	if err := st.ReplaceKeyAuthBindings(ctx, key.ID, []KeyAuthBinding{{Provider: "codex", AuthID: "account-a"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeletePluginKey(ctx, key.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := st.ListKeyAuthBindings(ctx, key.ID); err != nil || len(got) != 0 {
		t.Fatalf("bindings after delete = %#v err=%v", got, err)
	}
}
