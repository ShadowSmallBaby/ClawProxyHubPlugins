package main

import "testing"

// TestFingerprintDeterministic 同一 apiKey 必须派生同一台设备（重启/多实例一致性）。
func TestFingerprintDeterministic(t *testing.T) {
	prof := defaultDeviceProfile("")
	a := generateFingerprint("user_abc123", "", prof)
	b := generateFingerprint("user_abc123", "", prof)
	if a.Thumbmark != b.Thumbmark {
		t.Fatalf("thumbmark not stable: %s vs %s", a.Thumbmark, b.Thumbmark)
	}
	if a.Components.CPUModel != b.Components.CPUModel || a.Components.MachineIDHash != b.Components.MachineIDHash {
		t.Fatal("components not stable across calls")
	}
	if a.Thumbmark == "" || a.Components.CPUModel == "" {
		t.Fatal("empty fingerprint")
	}
}

// TestFingerprintKeyIsolation 不同 key 应派生不同设备（指纹随 key 走）。
func TestFingerprintKeyIsolation(t *testing.T) {
	prof := defaultDeviceProfile("")
	a := generateFingerprint("user_aaa", "", prof)
	b := generateFingerprint("user_bbb", "", prof)
	if a.Thumbmark == b.Thumbmark {
		t.Fatal("distinct keys produced identical thumbmark")
	}
}

// TestSaltRotatesIdentity 加盐后同一 key 换设备（成批更换身份的逃生口）。
func TestSaltRotatesIdentity(t *testing.T) {
	prof := defaultDeviceProfile("")
	a := generateFingerprint("user_abc123", "", prof)
	b := generateFingerprint("user_abc123", "salt-v2", prof)
	if a.Thumbmark == b.Thumbmark {
		t.Fatal("salt did not rotate device identity")
	}
}

// TestSlugify CLI slug 规则。
func TestSlugify(t *testing.T) {
	cases := map[string]string{
		`C:\Users\dev\projects\app`: "c-users-dev-projects-app",
		"":                          "root",
		"---":                       "root",
	}
	for in, want := range cases {
		if got := slugifyProjectPath(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}
