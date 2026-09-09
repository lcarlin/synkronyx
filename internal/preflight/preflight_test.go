// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 Luiz Antonio Carlin

package preflight

import (
	"os"
	"testing"
)

func TestMountOfFindsLongestPrefix(t *testing.T) {
	// A raiz sempre existe como montagem; qualquer path resolve para alguma.
	m, err := MountOf(t.TempDir())
	if err != nil {
		t.Fatalf("MountOf: %v", err)
	}
	if m.Point == "" || m.FSType == "" {
		t.Errorf("montagem incompleta: %+v", m)
	}
}

func TestMountOfRoot(t *testing.T) {
	m, err := MountOf("/")
	if err != nil {
		t.Fatal(err)
	}
	if m.Point != "/" {
		t.Errorf("Point = %q, quero \"/\"", m.Point)
	}
}

func TestIsNetworkClassifies(t *testing.T) {
	cases := map[string]bool{
		"nfs4":       true,
		"cifs":       true,
		"fuse.sshfs": true,
		"ext4":       false,
		"btrfs":      false,
		"tmpfs":      false,
		"overlay":    false,
	}
	for fstype, want := range cases {
		if got := (Mount{FSType: fstype}).IsNetwork(); got != want {
			t.Errorf("IsNetwork(%q) = %v, quero %v", fstype, got, want)
		}
	}
}

func TestWantsOwnership(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"--archive", "--partial"}, true},
		{[]string{"-a"}, true},
		{[]string{"-avz"}, true},
		{[]string{"--owner"}, true},
		{[]string{"-o"}, true},
		{[]string{"--recursive", "--times"}, false},
		{[]string{"-rt"}, false},
		{nil, false},
	}
	for _, c := range cases {
		if got := WantsOwnership(c.args); got != c.want {
			t.Errorf("WantsOwnership(%v) = %v, quero %v", c.args, got, c.want)
		}
	}
}

// O kernel escapa espaços e tabs nos campos do mountinfo; ler o campo cru
// quebraria em qualquer ponto de montagem com espaço no nome.
func TestUnescapeOctal(t *testing.T) {
	cases := map[string]string{
		`/mnt/disco`:              "/mnt/disco",
		`/mnt/meu\040disco`:       "/mnt/meu disco",
		`/mnt/a\011b`:             "/mnt/a\tb",
		`/mnt/barra\134invertida`: `/mnt/barra\invertida`,
	}
	for in, want := range cases {
		if got := unescapeOctal(in); got != want {
			t.Errorf("unescapeOctal(%q) = %q, quero %q", in, got, want)
		}
	}
}

func TestInotifyLimitIsReadable(t *testing.T) {
	n, err := InotifyLimit()
	if err != nil {
		t.Skipf("max_user_watches indisponível: %v", err)
	}
	if n <= 0 {
		t.Errorf("limite = %d, quero > 0", n)
	}
}

func TestWatchPressure(t *testing.T) {
	limit, err := InotifyLimit()
	if err != nil {
		t.Skip("max_user_watches indisponível")
	}
	if got := WatchPressure(limit); got < 0.99 {
		t.Errorf("WatchPressure(limite) = %.2f, quero ~1", got)
	}
	if got := WatchPressure(0); got != 0 {
		t.Errorf("WatchPressure(0) = %.2f, quero 0", got)
	}
}

func TestPreservesOwnershipMatchesEuid(t *testing.T) {
	if got, want := PreservesOwnership(), os.Geteuid() == 0; got != want {
		t.Errorf("PreservesOwnership() = %v, quero %v", got, want)
	}
}
