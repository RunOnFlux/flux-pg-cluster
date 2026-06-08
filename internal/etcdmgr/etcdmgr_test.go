package etcdmgr

import (
	"testing"
)

// v3 member list output (etcd v3.5, comma-space-separated 6 fields):
// <hex-id>, started|unstarted, <name>, <peerURLs>, <clientURLs>, <isLearner>

func TestParseMemberList_ActiveAndUnstarted(t *testing.T) {
	in := `7d0225fa214aabdb, started, node-172-20-0-10, https://172.20.0.10:2380, https://172.20.0.10:2379, false
98583ded33e2a2c, unstarted, , https://172.20.0.13:2380, , false
abcdef0123456789, started, node-172-20-0-14, https://172.20.0.14:2380, https://172.20.0.14:2379, false`

	ms := ParseMemberList(in)
	if len(ms) != 3 {
		t.Fatalf("expected 3 members, got %d", len(ms))
	}
	if ms[0].Name != "node-172-20-0-10" || ms[0].Unstarted || ms[0].IsLearner {
		t.Errorf("active voter parse wrong: %+v", ms[0])
	}
	if !ms[1].Unstarted || ms[1].Name != "" || ms[1].PeerURLs != "https://172.20.0.13:2380" {
		t.Errorf("unstarted parse wrong: %+v", ms[1])
	}
	if ms[2].Name != "node-172-20-0-14" || ms[2].Unstarted || ms[2].IsLearner {
		t.Errorf("active follower parse wrong: %+v", ms[2])
	}
}

func TestParseMemberList_LearnerMember(t *testing.T) {
	// etcd v3.5: field 5 is isLearner (true for learners, false for voters)
	in := `7d0225fa214aabdb, started, node-172-20-0-10, https://172.20.0.10:2380, https://172.20.0.10:2379, false
98583ded33e2a2c, started, node-172-20-0-11, https://172.20.0.11:2380, https://172.20.0.11:2379, true`

	ms := ParseMemberList(in)
	if len(ms) != 2 {
		t.Fatalf("expected 2 members, got %d", len(ms))
	}
	if ms[0].IsLearner {
		t.Errorf("voter should not be learner: %+v", ms[0])
	}
	if !ms[1].IsLearner || ms[1].Unstarted {
		t.Errorf("learner parse wrong: %+v", ms[1])
	}
}

func TestParseMemberList_EmptyClientURLsTreatedAsGhost(t *testing.T) {
	in := `abc123, unstarted, , https://1.1.1.1:2380, , false`
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
