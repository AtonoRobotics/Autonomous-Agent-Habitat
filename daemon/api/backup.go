// Backup/restore routes over daemon/backup (§14). Both are operator-only:
// running pg_dump/pg_restore against the daemon's own store is squarely
// the "deterministic services commit" tier decision 9 reserves from
// autonomous agent action, the same rationale as extension mutations and
// account/credential writes.
package api

import (
	"io"
	"net/http"
	"os"
)

// handleBackup streams a pg_dump snapshot to a temp file first, rather
// than directly to the response body, so a pg_dump failure partway
// through still gets a clean error status — once bytes are written to an
// http.ResponseWriter, the status code and headers can no longer change.
func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request) {
	tmp, err := os.CreateTemp("", "amh-backup-*.pgdump")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, simpleResponse{Error: err.Error()})
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	if err := s.Backup.Backup(r.Context(), tmp); err != nil {
		writeJSON(w, http.StatusInternalServerError, simpleResponse{Error: err.Error()})
		return
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		writeJSON(w, http.StatusInternalServerError, simpleResponse{Error: err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="amh-backup.pgdump"`)
	io.Copy(w, tmp)
}

// handleRestore streams the request body straight into pg_restore's
// stdin — Restore's --single-transaction makes the operation itself
// atomic against the target, so there's no comparable need to buffer
// through a temp file first.
func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	if err := s.Backup.Restore(r.Context(), r.Body); err != nil {
		writeJSON(w, http.StatusBadRequest, simpleResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, simpleResponse{})
}
