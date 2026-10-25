//go:build windows

package ajean

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// sys_service_windows_extservice.go — EXTERNAL_SERVICE sous Windows,
// l'équivalent du pilote systemd de sys_service_linux.go (unitAction,
// waitGPUsReleased, unitPIDs, unitActiveState). Un preset externe peut nommer
// un service local (« ajean-<nom> ») ; AJEAN le démarre à la bascule sur le
// preset, l'arrête quand on en repart, et attend la libération de la VRAM avant
// de charger le modèle suivant — exactement le contrat des unités systemd, sans
// systemd.
//
// La commande vit DANS le preset, comme BIN/MODEL le sont pour le moteur local
// (aucun fichier à côté) :
//
//	EXTERNAL_SERVICE=ajean-strata
//	SERVICE_BIN=I:\Strata\.venv\Scripts\python.exe
//	SERVICE_ARGS=I:\Strata\serve\server.py --engine strata --config I:\Strata\strata-swift-iq2_xs.json --port 8080
//	SERVICE_CWD=I:\Strata
//
// SERVICE_BIN (obligatoire) : le binaire à lancer. SERVICE_ARGS (optionnel) :
// ses arguments, découpés comme EXTRA_ARGS (espaces, guillemets respectés).
// SERVICE_CWD (optionnel) : le répertoire de travail. BIN/CWD relatifs se
// résolvent depuis $AJEAN_HOME. Le processus est lancé détaché (comme
// `ajean serve`), suivi par un fichier PID, journalisé dans $AJEAN_HOME, arrêté
// par taskkill /T /F : mêmes primitives que le superviseur local, sans droits
// admin.
//
// applyPresetFile écrit la config du preset cible AVANT que syncExternalService
// soit appelé, donc ReadConfig() porte déjà les clés du preset en cours de
// bascule : la plateforme les lit directement, sans autre source.

var extMu sync.Mutex

const (
	extKeyBin  = "SERVICE_BIN"
	extKeyArgs = "SERVICE_ARGS"
	extKeyCwd  = "SERVICE_CWD"
)

// extPidPath / extLogPath : suivi et journal, à côté de ceux du moteur local
// (ajean-engine.pid / ajean-engine.log), un fichier par service.
func extPidPath(unit string) string { return filepath.Join(AjeanHome(), unit+".pid") }
func extLogPath(unit string) string { return filepath.Join(AjeanHome(), unit+".log") }

// extServiceCmd lit la commande du service externe depuis le preset actif.
func extServiceCmd() (bin string, args []string, cwd string, err error) {
	cfg := ReadConfig()
	bin = strings.TrimSpace(cfg[extKeyBin])
	if bin == "" {
		return "", nil, "", fmt.Errorf("%s est défini mais %s manque (le binaire à lancer)", extKeyService, extKeyBin)
	}
	if !filepath.IsAbs(bin) {
		bin = filepath.Join(AjeanHome(), bin)
	}
	args = splitArgs(cfg[extKeyArgs])
	cwd = strings.TrimSpace(cfg[extKeyCwd])
	if cwd != "" && !filepath.IsAbs(cwd) {
		cwd = filepath.Join(AjeanHome(), cwd)
	}
	return bin, args, cwd, nil
}

// unitAction : start/stop d'un service externe. syncExternalService
// (backend_extservice.go) ne connaît que ces deux actions.
func unitAction(unit, action string) error {
	switch action {
	case "start":
		return extUnitStart(unit)
	case "stop":
		return extUnitStop(unit)
	}
	return fmt.Errorf("action inconnue : %s", action)
}

func extUnitStart(unit string) error {
	extMu.Lock()
	defer extMu.Unlock()
	if pid := extUnitPID(unit); pid > 0 {
		return nil // idempotent : syncExternalService relance start à chaque bascule
	}
	bin, args, cwd, err := extServiceCmd()
	if err != nil {
		return err
	}
	// Journal : best-effort, comme openEngineLog — un log inaccessible ne doit
	// PAS faire échouer le démarrage (issue #88).
	var logf *os.File
	if f, err := os.OpenFile(extLogPath(unit), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		logf = f
	}
	defer func() {
		if logf != nil {
			logf.Close()
		}
	}()

	cmd := exec.Command(bin, args...)
	if logf != nil {
		cmd.Stdout = logf
		cmd.Stderr = logf
	}
	if cwd != "" {
		cmd.Dir = cwd
	}
	// Détaché, sans console : le service survit à la fermeture de l'UI et ne
	// doit jamais faire clignoter de fenêtre noire (même attr que svcStart).
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNewProcessGroup | detachedProcess | createNoWindow,
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("démarrage de %s : %w", unit, err)
	}
	pid := cmd.Process.Pid
	// PID en best-effort : un échec d'écriture dégrade stop/status mais ne
	// casse pas un moteur déjà lancé.
	if err := os.WriteFile(extPidPath(unit), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		fmt.Printf("%s PID de %s non enregistré (%v)\n", yellow("[avert]"), unit, err)
	}
	_ = cmd.Process.Release()

	// 2 s comme svcStart : trop court pour un gros modèle (Strata charge en une
	// minute ou plus), juste assez pour attraper une vraie erreur de lancement.
	// externalServiceReady est le juge de paix (unité active + API qui répond).
	time.Sleep(2 * time.Second)
	if !processAlive(pid) {
		fmt.Printf("%s %s : le processus s'est arrêté — derniers logs :\n", red("[ERREUR]"), unit)
		fmt.Println("------------------------------------------------")
		fmt.Print(tailFile(extLogPath(unit), 20))
		fmt.Println("------------------------------------------------")
		_ = os.Remove(extPidPath(unit))
		return fmt.Errorf("service %s non démarré", unit)
	}
	return nil
}

func extUnitStop(unit string) error {
	pid := extUnitPID(unit)
	if pid == 0 {
		_ = os.Remove(extPidPath(unit))
		return nil // rien à arrêter : idempotent
	}
	// taskkill /T : le process ET ses enfants (python → moteur → workers).
	cmd := hideCmd(exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F"))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("arrêt de %s (PID %d): %w\n%s", unit, pid, err, string(out))
	}
	// taskkill /F rend la main AVANT la destruction effective : la VRAM reste
	// verrouillée par le pilote un instant. waitGPUsReleased (appelé juste après
	// par syncExternalService) couvre l'intervalle ; on ne bloque pas ici.
	_ = os.Remove(extPidPath(unit))
	return nil
}

// extUnitPID : le PID du service, 0 s'il ne tourne pas.
func extUnitPID(unit string) int {
	b, err := os.ReadFile(extPidPath(unit))
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid <= 0 || !processAlive(pid) {
		return 0
	}
	return pid
}

// unitActiveState : « active » si le process tourne, « failed » s'il s'est
// arrêté (PID connu mais mort), « inactive » sinon.
func unitActiveState(unit string) string {
	b, err := os.ReadFile(extPidPath(unit))
	if err != nil {
		return "inactive"
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid > 0 {
		if processAlive(pid) {
			return "active"
		}
		return "failed"
	}
	return "inactive"
}

// unitPIDs : le PID du service (nil s'il ne tourne pas). Relevé AVANT le stop
// pour que waitGPUsReleased sache quoi attendre.
func unitPIDs(unit string) []int {
	pid := extUnitPID(unit)
	if pid == 0 {
		return nil
	}
	return []int{pid}
}

// waitGPUsReleased : porté de sys_service_linux.go — nvidia-smi existe sous
// Windows, même interrogation. On ne regarde QUE les pids donnés : un autre
// programme GPU permanent ne doit pas faire attendre le délai entier.
func waitGPUsReleased(pids []int, max time.Duration) {
	if _, err := exec.LookPath("nvidia-smi"); err != nil {
		return
	}
	watch := map[string]bool{}
	for _, p := range pids {
		watch[strconv.Itoa(p)] = true
	}
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		out, err := exec.Command("nvidia-smi", "--query-compute-apps=pid", "--format=csv,noheader").Output()
		if err != nil {
			return
		}
		busy := false
		for _, l := range strings.Split(string(out), "\n") {
			l = strings.TrimSpace(l)
			if l != "" && (len(watch) == 0 || watch[l]) {
				busy = true
				break
			}
		}
		if !busy {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}
