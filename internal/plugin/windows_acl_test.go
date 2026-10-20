//go:build windows

package plugin

// Этот файл не реализует Windows-бэкенд. Он фиксирует измерением то, что
// SECURITY.md теперь утверждает: запись PROTECTED DACL на каталог, который
// процесс создал сам, непривилегированному процессу доступна, и она реально
// отрезает его собственный токен. Раньше здесь стояло «host ACL state» как
// блокер — измерение показало, что блокер другой (уровень целостности), а
// сам DACL работает. Утверждение без теста снова может устареть.

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/wykserdex/fidespipe/internal/pipeline"
)

// allApplicationPackages — S-1-15-2, SID, под которым ходят все AppContainer-токены.
const allApplicationPackages = "S-1-15-2"

func tHelperSID(t *testing.T, wellKnown func() (*windows.SID, error)) *windows.SID {
	t.Helper()
	sid, err := wellKnown()
	if err != nil {
		t.Skipf("SID недоступен: %v", err)
	}
	return sid
}

func grantAce(sid *windows.SID) windows.EXPLICIT_ACCESS {
	return windows.EXPLICIT_ACCESS{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       0,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}
}

// setDACL — ставит DACL, по умолчанию рвёт наследование (PROTECTED).
func setDACL(t *testing.T, path string, protect bool, sids ...*windows.SID) error {
	t.Helper()
	entries := make([]windows.EXPLICIT_ACCESS, 0, len(sids))
	for _, s := range sids {
		entries = append(entries, grantAce(s))
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return err
	}
	si := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION)
	if protect {
		si |= windows.PROTECTED_DACL_SECURITY_INFORMATION
	} else {
		si |= windows.UNPROTECTED_DACL_SECURITY_INFORMATION
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, si, nil, nil, acl, nil)
}

// currentUserSID — SID пользователя этого процесса.
func currentUserSID(t *testing.T) *windows.SID {
	t.Helper()
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || tu == nil || tu.User.Sid == nil {
		t.Skipf("SID пользователя недоступен: %v", err)
	}
	return tu.User.Sid
}

// TestWindowsSelfCreatedDirDACLIsRestrictable — центральное утверждение
// SECURITY.md. Непривилегированный процесс обязан сузить DACL собственного
// каталога и отрезать себе же доступ.
func TestWindowsSelfCreatedDirDACLIsRestrictable(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "sandbox")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	sys := tHelperSID(t, func() (*windows.SID, error) { return windows.CreateWellKnownSid(windows.WinLocalSystemSid) })
	adm := tHelperSID(t, func() (*windows.SID, error) {
		return windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	})
	app, err := windows.StringToSid(allApplicationPackages)
	if err != nil {
		t.Skipf("S-1-15-2 недоступен: %v", err)
	}

	user := currentUserSID(t)

	// Восстановление обязательно: без него упавший тест оставил бы каталог
	// закрытым, а это уже ломает машину, на которой тест запущен.
	defer func() {
		back := []*windows.SID{sys, adm}
		if user != nil {
			back = append(back, user)
		}
		_ = setDACL(t, dir, false, back...)
	}()

	// До правки доступ должен быть.
	if _, err := os.ReadDir(dir); err != nil {
		t.Fatalf("до записи DACL каталог должен быть доступен: %v", err)
	}

	// PROTECTED — несущий флаг: он рвёт наследование.
	if err := setDACL(t, dir, true, sys, adm, app); err != nil {
		t.Fatalf("SetNamedSecurityInfo на собственном каталоге обязан работать без повышения, получено: %v", err)
	}

	// Проверяем структурно, а не «ReadDir падает». Группе Administrators
	// выдан GENERIC_ALL, поэтому у процесса, который сам состоит в админах и
	// работает с повышенным токеном, доступ законно остаётся через этот ACE —
	// так и должно быть, повышенный процесс в любом случае может вернуть
	// каталог себе. Блокируется именно непривилегированный токен: у него SID
	// Administrators в deny-only, и тогда остаётся только SYSTEM/Administrators/
	// S-1-15-2, а пользовательского SID в DACL нет вовсе.
	if aclGrants(t, dir, user) {
		t.Fatal("после PROTECTED DACL SID пользователя не должен остаться в DACL")
	}

	if !elevated() {
		if _, err := os.ReadDir(dir); err == nil {
			t.Error("непривилегированный токен должен быть отрезан, но доступ сохранился")
		}
	}
}

// aclGrants — есть ли в DACL пути прямой выдачи указанному SID.
func aclGrants(t *testing.T, path string, sid *windows.SID) bool {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("GetNamedSecurityInfo: %v", err)
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil {
		return false
	}
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil {
			continue
		}
		// SID в ACCESS_ALLOWED_ACE начинается сразу за маской, то есть по
		// адресу SidStart. Заголовок SID и ACCESS_ALLOWED_ACE совместимы по
		// раскладке, поэтому указатель кастуется напрямую.
		aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if aceSID == nil || aceSID.SubAuthorityCount() > 15 {
			continue
		}
		if aceSID.String() == sid.String() {
			return true
		}
	}
	return false
}

func elevated() bool {
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &tok); err != nil {
		return false
	}
	defer tok.Close()
	return tok.IsElevated()
}

// TestWindowsDACLWithoutProtectedKeepsUserAccess — контроль: флаг
// PROTECTED_DACL_SECURITY_INFORMATION несущий. Без него новый ACL сливается с
// унаследованными ACE, и ограничения не возникает.
func TestWindowsDACLWithoutProtectedKeepsUserAccess(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "sandbox")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	sys := tHelperSID(t, func() (*windows.SID, error) { return windows.CreateWellKnownSid(windows.WinLocalSystemSid) })
	adm := tHelperSID(t, func() (*windows.SID, error) {
		return windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	})
	app, err := windows.StringToSid(allApplicationPackages)
	if err != nil {
		t.Skipf("S-1-15-2 недоступен: %v", err)
	}
	user := currentUserSID(t)
	defer func() {
		back := []*windows.SID{sys, adm}
		if user != nil {
			back = append(back, user)
		}
		_ = setDACL(t, dir, false, back...)
	}()

	if err := setDACL(t, dir, false, sys, adm, app); err != nil {
		t.Fatalf("SetNamedSecurityInfo без PROTECTED: %v", err)
	}
	if _, err := os.ReadDir(dir); err != nil {
		t.Fatalf("без PROTECTED наследование должно сохранить доступ пользователя, получено: %v", err)
	}
}

// TestWindowsNoWindowsBackendRemainsFailClosed — изоляция на Windows не
// появилась: этот тест фиксирует, что бэкенда нет и ядро обязано отказать.
// Если когда-нибудь появится настоящий AppContainer-бэкенд, тест напомнит
// обновить SECURITY.md.
func TestWindowsNoWindowsBackendRemainsFailClosed(t *testing.T) {
	if _, ok := sandboxBackend(); ok {
		t.Skip("Windows-бэкенд появился — обновите SECURITY.md и этот тест")
	}
	m := &pipeline.Manifest{
		ID:      "x",
		Runtime: pipeline.Runtime{Type: "python", Entry: "plugin.py"},
		Dir:     t.TempDir(),
	}
	if _, _, _, err := sandboxArgs(m, []string{"/bin/sh", "-c", "true"}, t.TempDir()); err == nil {
		t.Fatal("без Windows-бэкенда запуск внешнего кода обязан быть отказ, а не разрешён")
	}
}
