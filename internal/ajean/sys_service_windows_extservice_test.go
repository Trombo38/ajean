//go:build windows

package ajean

import (
	"os"
	"strconv"
	"testing"
)

// Tests Windows des services externes (EXTERNAL_SERVICE) : le suivi PID, la
// lecture de la commande depuis le preset, et l'exclusivité GPU obtenue par
// l'ordre des appels — l'équivalent des garanties systemd sous Linux.

func TestExtUnitStateTracking(t *testing.T) {
	testHome(t)
	// jamais démarré → inactive, pas de PIDs
	if s := unitActiveState("ajean-strata"); s != "inactive" {
		t.Fatalf("état initial : %q", s)
	}
	if pids := unitPIDs("ajean-strata"); pids != nil {
		t.Fatalf("pids initial : %v", pids)
	}
	if pid := extUnitPID("ajean-strata"); pid != 0 {
		t.Fatalf("pid initial : %d", pid)
	}

	// PID vivant (ce processus même) → active, pids connus
	setExtPID("ajean-strata", os.Getpid())
	if s := unitActiveState("ajean-strata"); s != "active" {
		t.Fatalf("état vivant : %q", s)
	}
	if pids := unitPIDs("ajean-strata"); len(pids) != 1 || pids[0] != os.Getpid() {
		t.Fatalf("pids vivants : %v", pids)
	}

	// PID mort → failed (le service s'est arrêté)
	setExtPID("ajean-strata", 99999999)
	if s := unitActiveState("ajean-strata"); s != "failed" {
		t.Fatalf("état mort : %q", s)
	}
}

func setExtPID(unit string, pid int) {
	if pid > 0 {
		if err := os.WriteFile(extPidPath(unit), []byte(strconv.Itoa(pid)), 0o644); err != nil {
			panic(err)
		}
	} else {
		_ = os.Remove(extPidPath(unit))
	}
}

// La commande est lue du preset actif (SERVICE_BIN/ARGS/CWD), pas d'un fichier
// à côté : BIN relatif résolu depuis $AJEAN_HOME, ARGS découpés comme
// EXTRA_ARGS (guillemets respectés), CWD optionnel.
func TestExtServiceCmdFromPreset(t *testing.T) {
	home := testHome(t)
	defer os.Remove(extPidPath("ajean-strata"))

	// preset sans SERVICE_BIN : erreur lisible
	if _, _, _, err := extServiceCmd(); err == nil {
		t.Fatal("SERVICE_BIN absent accepté")
	}

	// preset complet
	cfg := map[string]string{
		extKeyService: "ajean-strata",
		extKeyBin:     `bin\serve.exe`,
		extKeyArgs:    `--port 8080 --config "I:\Strata\mon config.json"`,
		extKeyCwd:     `strata`,
	}
	if err := WriteConfig(cfg); err != nil {
		t.Fatal(err)
	}
	bin, args, cwd, err := extServiceCmd()
	if err != nil {
		t.Fatalf("parsing : %v", err)
	}
	if want := home + `\bin\serve.exe`; bin != want {
		t.Fatalf("BIN relatif : %q (attendu %q)", bin, want)
	}
	if want := home + `\strata`; cwd != want {
		t.Fatalf("CWD relatif : %q (attendu %q)", cwd, want)
	}
	// 3 args : --port, 8080, --config, "I:\Strata\mon config.json" → 4 en fait
	wantArgs := []string{"--port", "8080", "--config", `I:\Strata\mon config.json`}
	if len(args) != len(wantArgs) {
		t.Fatalf("args : %v (attendu %v)", args, wantArgs)
	}
	for i := range wantArgs {
		if args[i] != wantArgs[i] {
			t.Fatalf("args[%d] = %q (attendu %q)", i, args[i], wantArgs[i])
		}
	}
}

// L'exclusivité GPU : le flux de bascule (SwitchToPreset) arrête TOUJOURS un
// moteur avant d'en démarrer un autre. Testé à travers syncExternalService,
// comme sous Linux.
func TestWindowsExternalServiceMutex(t *testing.T) {
	testHome(t)
	var calls []string
	realAction := unitAction
	extUnitAction = func(unit, action string) error {
		calls = append(calls, action+" "+unit)
		if action == "start" {
			// le « process » est ce test même : vivant
			setExtPID(unit, os.Getpid())
			return nil
		}
		return os.Remove(extPidPath(unit))
	}
	defer func() { extUnitAction = realAction }()

	steps := []struct {
		cfg  map[string]string
		want []string
	}{
		// bascule vers le preset Strata : le moteur tiers démarre
		{map[string]string{"EXTERNAL": "1", "EXTERNAL_URL": "http://127.0.0.1:8080/v1", extKeyService: "ajean-strata"},
			[]string{"start ajean-strata"}},
		// re-bascule : idempotent
		{map[string]string{"EXTERNAL": "1", "EXTERNAL_URL": "http://127.0.0.1:8080/v1", extKeyService: "ajean-strata"},
			[]string{"start ajean-strata"}},
		// bascule vers un preset local : le moteur tiers s'arrête AVANT
		// que llama.cpp reparte
		{map[string]string{"MODEL": "x.gguf"}, []string{"stop ajean-strata"}},
		{map[string]string{"MODEL": "x.gguf"}, nil},
	}
	for i, s := range steps {
		calls = nil
		if err := syncExternalService(s.cfg); err != nil {
			t.Fatalf("étape %d: %v", i, err)
		}
		if len(calls) != len(s.want) {
			t.Fatalf("étape %d: appels %v, attendu %v", i, calls, s.want)
		}
		for j := range calls {
			if calls[j] != s.want[j] {
				t.Fatalf("étape %d: appels %v, attendu %v", i, calls, s.want)
			}
		}
	}

	// après arrêt : plus rien de suivi, état propre
	if s := unitActiveState("ajean-strata"); s != "inactive" {
		t.Fatalf("état après arrêt : %q", s)
	}
}
