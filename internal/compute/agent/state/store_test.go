package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestStore(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	put := func(rv, uid, body string) bool {
		t.Helper()
		ok, err := s.Put("LibvirtDomain", "ns/vm1", uid, rv, json.RawMessage(body))
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}

	if !put("10", "u1", `{"v":1}`) {
		t.Fatal("first put rejected")
	}
	if put("9", "u1", `{"v":0}`) || put("10", "u1", `{"v":1}`) {
		t.Fatal("older or equal resourceVersion accepted")
	}
	if !put("11", "u1", `{"v":2}`) {
		t.Fatal("newer resourceVersion rejected")
	}
	if err := s.Update("LibvirtDomain", "ns/vm1", func(r *Record) { r.AppliedHash, r.PoweredOff = "h", true }); err != nil {
		t.Fatal(err)
	}
	if !put("12", "u1", `{"v":3}`) || s.List("LibvirtDomain")[0].AppliedHash != "h" {
		t.Fatal("put must keep applied facts of the same object")
	}

	// 重启后从磁盘恢复，且 MarkDeleted 持久化。
	if err := s.MarkDeleted("LibvirtDomain", "ns/vm1"); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := s.List("LibvirtDomain")
	if len(got) != 1 || !got[0].Deleted || !got[0].PoweredOff || string(got[0].Object) != `{"v":3}` {
		t.Fatalf("reloaded record = %+v", got)
	}

	// 同名对象删除后重建（UID 不同）：旧对象的事实不得沿用，即使 resourceVersion 更小。
	if !put("5", "u2", `{"v":"new"}`) {
		t.Fatal("recreated object rejected")
	}
	if r := s.List("LibvirtDomain")[0]; r.UID != "u2" || r.Deleted || r.PoweredOff || r.AppliedHash != "" {
		t.Fatalf("recreated record kept old facts: %+v", r)
	}

	if err := s.Forget("LibvirtDomain", "ns/vm1"); err != nil {
		t.Fatal(err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*")); len(left) != 0 {
		t.Fatalf("files left after Forget: %v", left)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
}
