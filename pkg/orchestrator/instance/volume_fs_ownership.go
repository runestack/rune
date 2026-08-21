package instance

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
)

// applyFSOwnership chowns / chmods path according to the operator's
// VolumeMount.FSUser / FSGroup / FSMode opt-in. Each step is idempotent
// — current ownership / mode is compared first and the syscall is
// skipped on match. Absent fields (nil user, nil group, empty mode)
// are no-ops, so volumes whose VolumeMount declares none of these
// behave exactly as before (relevant for local-host paths the operator
// manages directly).
//
// Only the mount root is touched. SubPath ownership is the operator's
// responsibility — either via image entrypoint or initSteps — to keep
// this helper from walking arbitrary trees the operator may have
// populated.
func applyFSOwnership(path string, fsUser, fsGroup *int, fsMode string) error {
	if fsUser == nil && fsGroup == nil && fsMode == "" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}

	if fsUser != nil || fsGroup != nil {
		curUID, curGID, ok := unixOwner(info)
		if !ok {
			// Non-unix platforms (darwin tests can reach here via fakeMounter
			// + a tmpdir source). Skip silently — operators only set fsUser/
			// fsGroup on the real Linux production agent.
			goto chmodStep
		}
		wantUID := curUID
		wantGID := curGID
		if fsUser != nil {
			wantUID = *fsUser
		}
		if fsGroup != nil {
			wantGID = *fsGroup
		}
		if wantUID != curUID || wantGID != curGID {
			// os.Chown accepts -1 as "leave unchanged" via syscall.Chown,
			// but since we just read the current ids we can pass them
			// through unconditionally and the result is the same.
			if err := os.Chown(path, wantUID, wantGID); err != nil {
				return fmt.Errorf("chown %d:%d: %w", wantUID, wantGID, err)
			}
		}
	}

chmodStep:
	if fsMode != "" {
		want, err := strconv.ParseUint(fsMode, 8, 32)
		if err != nil {
			return fmt.Errorf("parse fsMode %q (expected octal like \"0775\"): %w", fsMode, err)
		}
		wantPerm := os.FileMode(want) & os.ModePerm
		curPerm := info.Mode() & os.ModePerm
		if wantPerm != curPerm {
			if err := os.Chmod(path, wantPerm); err != nil {
				return fmt.Errorf("chmod %o: %w", wantPerm, err)
			}
		}
	}
	return nil
}

// unixOwner extracts the unix uid/gid from a FileInfo. Returns ok=false
// on non-unix file systems (Windows). The agent only runs on Linux in
// production; this guard keeps the function callable from darwin unit
// tests without compile errors.
func unixOwner(info os.FileInfo) (uid, gid int, ok bool) {
	st, isUnix := info.Sys().(*syscall.Stat_t)
	if !isUnix {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}
