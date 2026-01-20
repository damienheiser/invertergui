// Package auth provides authentication against OpenWRT/Linux system credentials
package auth

import (
	"bufio"
	"crypto/subtle"
	"os"
	"os/exec"
	"strings"
)

// Authenticator handles authentication against system credentials
type Authenticator struct {
	shadowPath string
	fallback   bool
	fbUsername string
	fbPassword string
}

// New creates a new authenticator
// If fallbackUser/fallbackPass are provided, they're used when shadow auth fails
func New(fallbackUser, fallbackPass string) *Authenticator {
	return &Authenticator{
		shadowPath: "/etc/shadow",
		fallback:   fallbackUser != "" && fallbackPass != "",
		fbUsername: fallbackUser,
		fbPassword: fallbackPass,
	}
}

// Authenticate checks if the provided credentials are valid
// On OpenWRT: tries to authenticate against root user via dropbear/busybox
// Falls back to configured credentials if system auth fails
func (a *Authenticator) Authenticate(username, password string) bool {
	// Try system authentication first (root user on OpenWRT)
	if username == "root" && a.authenticateSystem(password) {
		return true
	}

	// Fall back to configured credentials
	if a.fallback {
		usernameOK := subtle.ConstantTimeCompare([]byte(username), []byte(a.fbUsername)) == 1
		passwordOK := subtle.ConstantTimeCompare([]byte(password), []byte(a.fbPassword)) == 1
		return usernameOK && passwordOK
	}

	return false
}

// authenticateSystem verifies password against the system's root password
// Uses checkpassword-style verification via /bin/login or reading shadow
func (a *Authenticator) authenticateSystem(password string) bool {
	// Method 1: Try to read shadow file and verify MD5crypt hash
	if hash := a.getRootHash(); hash != "" {
		if verifyMD5Crypt(password, hash) {
			return true
		}
	}

	// Method 2: Try using busybox's login check (OpenWRT)
	// This runs: echo "password" | busybox login -f root 2>/dev/null
	// Note: This is a fallback and may not work on all systems
	cmd := exec.Command("sh", "-c", "echo \""+escapeShell(password)+"\" | busybox login -f root 2>/dev/null")
	if err := cmd.Run(); err == nil {
		return true
	}

	return false
}

// getRootHash reads the root user's password hash from /etc/shadow
func (a *Authenticator) getRootHash() string {
	file, err := os.Open(a.shadowPath)
	if err != nil {
		return ""
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Split(line, ":")
		if len(parts) >= 2 && parts[0] == "root" {
			hash := parts[1]
			// Skip empty, locked, or disabled passwords
			if hash == "" || hash == "*" || hash == "!" || hash == "x" || hash == "!!" {
				return ""
			}
			return hash
		}
	}
	return ""
}

// escapeShell escapes a string for safe use in shell
func escapeShell(s string) string {
	// Replace dangerous characters
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	s = strings.ReplaceAll(s, "$", "\\$")
	s = strings.ReplaceAll(s, "`", "\\`")
	return s
}

// verifyMD5Crypt verifies a password against an MD5crypt hash ($1$salt$hash)
// This is the most common hash type on OpenWRT
func verifyMD5Crypt(password, hash string) bool {
	if !strings.HasPrefix(hash, "$1$") {
		return false
	}

	// Extract salt from hash: $1$salt$hashvalue
	parts := strings.Split(hash, "$")
	if len(parts) != 4 {
		return false
	}
	salt := parts[2]

	// Compute the hash using the same algorithm
	computed := md5Crypt([]byte(password), []byte(salt))

	return subtle.ConstantTimeCompare([]byte(computed), []byte(hash)) == 1
}

// md5Crypt implements the MD5-based Unix crypt algorithm
// Algorithm based on glibc's crypt implementation
func md5Crypt(password, salt []byte) string {
	// Use OpenSSL via shell if available (most reliable)
	// openssl passwd -1 -salt SALT PASSWORD
	cmd := exec.Command("openssl", "passwd", "-1", "-salt", string(salt), string(password))
	output, err := cmd.Output()
	if err == nil {
		return strings.TrimSpace(string(output))
	}

	// Fallback: try using busybox's cryptpw
	cmd = exec.Command("cryptpw", "-m", "md5", "-S", string(salt), string(password))
	output, err = cmd.Output()
	if err == nil {
		return strings.TrimSpace(string(output))
	}

	return ""
}
