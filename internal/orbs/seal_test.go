package orbs

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The agent decrypts with openssl in agent.sh, so seal must match it exactly.
func TestSealOpensWithAgent(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl not installed")
	}
	dir := t.TempDir()
	key := filepath.Join(dir, "key.pem")
	if out, err := exec.Command("openssl", "ecparam", "-name", "prime256v1", "-genkey", "-noout", "-out", key).CombinedOutput(); err != nil {
		t.Fatalf("genkey: %v\n%s", err, out)
	}
	pub, err := exec.Command("sh", "-c", "openssl pkey -in "+key+" -pubout -outform DER | base64 -w0").Output()
	if err != nil {
		t.Fatal(err)
	}
	agent := filepath.Join(dir, "agent.sh")
	if err := os.WriteFile(agent, []byte(agentScript), 0o755); err != nil {
		t.Fatal(err)
	}

	// Lengths on both sides of the AES block size, and a realistic token.
	for _, plaintext := range []string{
		"",
		"0123456789abcde",
		"0123456789abcdef",
		"9f8e7d6c5b4a3921\n" + strings.Repeat("tok.", 150) + "\nhttps://ampcode.com\n",
	} {
		sealed, err := seal(string(pub), []byte(plaintext))
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("bash", "-c", `. "$1" && open_payload "$2"`, "bash", agent, key)
		cmd.Stdin = strings.NewReader(sealed)
		got, err := cmd.Output()
		if err != nil {
			t.Fatalf("open_payload(%d bytes): %v", len(plaintext), err)
		}
		if string(got) != plaintext {
			t.Errorf("open_payload = %q, want %q", got, plaintext)
		}
	}
}
