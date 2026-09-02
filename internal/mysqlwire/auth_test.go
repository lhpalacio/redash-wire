package mysqlwire

import (
	"errors"
	"testing"

	"github.com/go-mysql-org/go-mysql/server"
)

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

	if b := newCredentialAuthHandler("alice", "secret", nil); b.decoy == a.decoy {
		t.Error("two handlers share a decoy; it must be random per connection")
	}

	// With an empty configured password, an unknown user still gets a decoy, so
	// an empty client password is not accepted for it.
	c := newCredentialAuthHandler("alice", "", nil)
	if cred, _, _ := c.GetCredential("mallory"); cred.Passwords[0] == "" {
		t.Error("unknown user was handed the empty password")
	}
}

func TestOnAuthSuccessRunsAdmit(t *testing.T) {
	if err := newCredentialAuthHandler("u", "p", nil).OnAuthSuccess(nil); err != nil {
		t.Errorf("OnAuthSuccess without admit = %v, want nil", err)
	}
	refused := errors.New("refused")
	a := newCredentialAuthHandler("u", "p", func(_ *server.Conn) error { return refused })
	if err := a.OnAuthSuccess(nil); !errors.Is(err, refused) {
		t.Errorf("OnAuthSuccess = %v, want admit's error to reach the client instead of OK", err)
	}
}
