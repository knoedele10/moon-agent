package model

import "testing"

func TestSelectDriverDefaults(t *testing.T) {
	t.Setenv("DB_DRIVER", "")
	t.Setenv("DB_HOST", "")
	t.Setenv("DB_USER", "")
	t.Setenv("DB_NAME", "")
	t.Setenv("DB_PASSWORD", "")
	got, err := SelectDriver()
	if err != nil || got != "sqlite" {
		t.Fatalf("clean env: got %q err %v", got, err)
	}

	t.Setenv("DB_HOST", "127.0.0.1")
	got, err = SelectDriver()
	if err != nil || got != "mysql" {
		t.Fatalf("legacy mysql env: got %q err %v", got, err)
	}

	t.Setenv("DB_DRIVER", "sqlite")
	got, err = SelectDriver()
	if err != nil || got != "sqlite" {
		t.Fatalf("explicit sqlite wins: got %q err %v", got, err)
	}

	t.Setenv("DB_DRIVER", "postgres")
	if _, err := SelectDriver(); err == nil {
		t.Fatal("unknown driver should fail")
	}
}
