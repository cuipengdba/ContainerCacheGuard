#!/usr/bin/env bash
# Run inside one PostgreSQL 14 backup container. All paths and credentials are
# provided by the deployment; no database server or DBS process is started.
set -Eeuo pipefail
umask 077

: "${BACKUP_ROOT:?set BACKUP_ROOT to the mounted backup volume}"
: "${PGHOST:?set PGHOST to the shared Unix socket directory}"
: "${PGPORT:?set PGPORT to the PostgreSQL port}"
: "${PGUSER:=replica}"
: "${MAX_RATE:=60M}"
BACKUP_NAME="${1:-$(date +%Y%m%d)}"

[[ "$BACKUP_ROOT" == /* && "$BACKUP_ROOT" != / && ! -L "$BACKUP_ROOT" && -d "$BACKUP_ROOT" ]] || {
  echo 'BACKUP_ROOT must be an existing absolute mounted directory' >&2; exit 2;
}
[[ "$PGHOST" == /* && -d "$PGHOST" && ! -L "$PGHOST" ]] || {
  echo 'PGHOST must be an existing absolute Unix socket directory' >&2; exit 2;
}
[[ "$PGPORT" =~ ^[0-9]{2,5}$ && "$MAX_RATE" =~ ^[0-9]+[kMG]$ ]] || {
  echo 'invalid PGPORT or MAX_RATE' >&2; exit 2;
}
[[ "$BACKUP_NAME" =~ ^[0-9]{8}(-[A-Za-z0-9._]+)?$ ]] || {
  echo 'backup name must start with YYYYMMDD' >&2; exit 2;
}
[[ "$(pg_basebackup --version)" == pg_basebackup\ \(PostgreSQL\)\ 14.* ]] || {
  echo 'PostgreSQL 14 pg_basebackup is required' >&2; exit 2;
}
[[ "$(pg_verifybackup --version)" == pg_verifybackup\ \(PostgreSQL\)\ 14.* ]] || {
  echo 'PostgreSQL 14 pg_verifybackup is required' >&2; exit 2;
}

exec 9>"$BACKUP_ROOT/.backup.lock"
flock -n 9 || { echo 'another backup is active' >&2; exit 3; }
final="$BACKUP_ROOT/$BACKUP_NAME"
work="$BACKUP_ROOT/.inprogress-$BACKUP_NAME-$$"
[[ ! -e "$final" && ! -e "$work" ]] || { echo 'backup destination already exists' >&2; exit 3; }
printf '%s role=%s state=start name=%s rate=%s\n' "$(date -Is)" "${ROLE:-unknown}" "$BACKUP_NAME" "$MAX_RATE"

if ! pg_basebackup -F p -P -v --checkpoint=spread -X s \
    -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" \
    -D "$work" --max-rate="$MAX_RATE"; then
  echo "pg_basebackup failed; incomplete files kept at $work" >&2
  exit 4
fi
[[ -s "$work/backup_manifest" && -s "$work/backup_label" ]] || {
  echo "backup manifest or label missing; incomplete files kept at $work" >&2; exit 5;
}
if ! pg_verifybackup "$work"; then
  echo "pg_verifybackup failed; files kept at $work" >&2
  exit 6
fi
mv -T -- "$work" "$final"
sha256sum "$final/backup_manifest" >"$BACKUP_ROOT/$BACKUP_NAME.manifest.sha256"
printf '%s role=%s state=verified name=%s path=%s\n' \
  "$(date -Is)" "${ROLE:-unknown}" "$BACKUP_NAME" "$final"
