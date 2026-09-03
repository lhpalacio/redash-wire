package mysqlwire

import "testing"

func TestGetCredentialNeverReportsAnUnknownUser(t *testing.T) {
	a := newCredentialAuthHandler("alice", "secret", nil)

	cred, found, err := a.GetCredential("alice")
	if err != nil || !found || len(cred.Passwords) != 1 || cred.Passwords[0] != "secret" {
		t.Fatalf("GetCredential(alice) = (%v, %v, %v), want the configured password", cred, found, err)
	}

	// go-mysql turns found=false into ER_NO_SUCH_USER, a username oracle. An
	// unknown user must look exactly like a known one with a wrong password.
	cred, found, err = a.GetCredential("mallory")
	if err != nil || !found {
		t.Fatalf("GetCredential(mallory) = (found %v, err %v), want found", found, err)
	}
	if len(cred.Passwords) != 1 || cred.Passwords[0] == "secret" || cred.Passwords[0] == "" {
		t.Errorf("GetCredential(mallory) passwords = %q, want one unguessable decoy", cred.Passwords)
	}

	// With an empty configured password, an unknown user still gets a decoy, so
	// an empty client password is not accepted for it.
	c := newCredentialAuthHandler("alice", "", nil)
	if cred, _, _ := c.GetCredential("mallory"); cred.Passwords[0] == "" {
		t.Error("unknown user was handed the empty password")
	}
}
