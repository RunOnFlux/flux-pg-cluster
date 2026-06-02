package etcdmgr

import (
	"testing"
)

func TestParseMemberList_ActiveAndUnstarted(t *testing.T) {
	in := `7d0225fa214aabdb: name=node-172-20-0-10 peerURLs=https://172.20.0.10:2380 clientURLs=https://172.20.0.10:2379 isLeader=true
98583ded33e2a2c[unstarted]: peerURLs=https://172.20.0.13:2380 clientURLs=
abcdef0123456789: name=node-172-20-0-14 peerURLs=https://172.20.0.14:2380 clientURLs=https://172.20.0.14:2379 isLeader=false`

	ms := ParseMemberList(in)
	if len(ms) != 3 {
		t.Fatalf("expected 3 members, got %d", len(ms))
	}
	if ms[0].Name != "node-172-20-0-10" || !ms[0].IsLeader || ms[0].Unstarted {
		t.Errorf("active leader parse wrong: %+v", ms[0])
	}
	if !ms[1].Unstarted || ms[1].Name != "" || ms[1].PeerURLs != "https://172.20.0.13:2380" {
		t.Errorf("unstarted parse wrong: %+v", ms[1])
	}
	if ms[2].Name != "node-172-20-0-14" || ms[2].Unstarted {
		t.Errorf("active follower parse wrong: %+v", ms[2])
	}
}

func TestParseMemberList_EmptyClientURLsTreatedAsGhost(t *testing.T) {
	in := `abc: name=node-1 peerURLs=https://1.1.1.1:2380 clientURLs= isLeader=false`
	ms := ParseMemberList(in)
	if len(ms) != 1 || !ms[0].Unstarted {
		t.Fatalf("expected ghost member, got %+v", ms)
	}
}

func TestParseMemberList_Empty(t *testing.T) {
	if got := ParseMemberList(""); len(got) != 0 {
		t.Fatalf("expected empty, got %v", got)
	}
}

func TestFindByPeerURL(t *testing.T) {
	ms := []Member{
		{ID: "a", PeerURLs: "https://1.1.1.1:2380"},
		{ID: "b", PeerURLs: "https://2.2.2.2:2380"},
	}
	if m := FindByPeerURL(ms, "https://2.2.2.2:2380"); m == nil || m.ID != "b" {
		t.Fatalf("expected b, got %+v", m)
	}
	if m := FindByPeerURL(ms, "https://9.9.9.9:2380"); m != nil {
		t.Fatalf("expected nil, got %+v", m)
	}
}
