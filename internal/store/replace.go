package store

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

// HardwareStore changes the hardware an inventory entry stands for. These are
// the only writers of the MAC set besides CreateDevice and the primary-MAC
// edit of UpdateDevice (model.Device.MACs): an explicit admin action.
type HardwareStore interface {
	// AdoptMAC records the MAC of a device that has none stored. It writes
	// only into an empty slot: a stored MAC is never overwritten.
	AdoptMAC(ctx context.Context, id model.DeviceID, mac string) error
	// SwapHardware moves the entry onto new hardware (see HardwareSwap) in
	// one transaction and returns the MAC set it had before.
	SwapHardware(ctx context.Context, id model.DeviceID, sw HardwareSwap) (oldMACs []string, err error)
}

// HardwareSwap is what an entry takes over from replacement hardware.
type HardwareSwap struct {
	// ExpectMACs is the set the entry must still hold; anything else means an
	// admin edited the entry while the replacement ran (ErrConflict).
	ExpectMACs []string
	MAC        string   // new primary MAC
	MACs       []string // further MACs of the new hardware (Ethernet)
	Address    string
	Port       int
	Identity   model.Identity
}

// AdoptMAC implements HardwareStore.
func (s *SQLite) AdoptMAC(ctx context.Context, id model.DeviceID, mac string) error {
	mac = strings.ToLower(strings.TrimSpace(mac))
	if mac == "" {
		return fmt.Errorf("%w: empty MAC", ErrConflict)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit; on failure the original error is returned
	res, err := tx.ExecContext(ctx, "UPDATE devices SET mac=?, updated_at=? WHERE id=? AND mac=''", mac, toMS(s.now()), id)
	if err != nil {
		return mapErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 { //nolint:errcheck // SQLite always reports it
		return fmt.Errorf("%w: device %d is gone or already has a MAC", ErrConflict, id)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO device_macs (mac, device_id) VALUES (?,?)", mac, id); err != nil {
		return mapErr(err)
	}
	return tx.Commit()
}

// SwapHardware implements HardwareStore. MAC set, primary MAC, address, port
// and identity change together or not at all: a MAC already bound to another
// device, an address taken by another entry or an entry that changed since
// ExpectMACs was read leaves the entry exactly as it was. Name, tags, notes,
// credentials, topic, history and backups stay.
func (s *SQLite) SwapHardware(ctx context.Context, id model.DeviceID, sw HardwareSwap) ([]string, error) {
	next := macSet(sw.MAC, sw.MACs)
	if len(next) == 0 {
		return nil, fmt.Errorf("%w: the new hardware has no MAC", ErrConflict)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit; on failure the original error is returned
	old, err := txMACs(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	want := macSet("", sw.ExpectMACs)
	slices.Sort(want)
	if !slices.Equal(old, want) {
		return nil, fmt.Errorf("%w: the entry's MACs changed while the replacement ran", ErrConflict)
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM device_macs WHERE device_id=?", id); err != nil {
		return nil, err
	}
	i := sw.Identity
	if err := affected(tx.ExecContext(ctx, `UPDATE devices SET mac=?, address=?, port=?, chip_family=?, hardware=?, single_core=?,
		fw_version=?, fw_image=?, fw_variant=?, fw_language=?, flash_size_kb=?, program_flash_kb=?, free_kb=?, identity_at=?, identity_lang_known=?,
		fw_reach='', fw_reach_at=NULL, fw_reach_detail='', up_layout='', up_layout_at=NULL, updated_at=? WHERE id=?`,
		next[0], sw.Address, sw.Port, string(i.Family), i.Hardware, b2i(i.SingleCore), i.Version, i.Image, i.Variant, i.Language,
		i.FlashSizeKB, i.ProgramFlashSizeKB, i.FreeKB, toMS(i.DetectedAt), b2i(i.LanguageKnown), toMS(s.now()), id)); err != nil {
		return nil, err
	}
	for _, m := range next {
		if _, err := tx.ExecContext(ctx, "INSERT INTO device_macs (mac, device_id) VALUES (?,?)", m, id); err != nil {
			return nil, mapErr(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return old, nil
}

// txMACs returns the entry's MAC set, sorted; an unknown entry is ErrNotFound.
func txMACs(ctx context.Context, tx *sql.Tx, id model.DeviceID) ([]string, error) {
	var one int
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM devices WHERE id=?", id).Scan(&one); err != nil {
		return nil, notFound(err)
	}
	rows, err := tx.QueryContext(ctx, "SELECT mac FROM device_macs WHERE device_id=? ORDER BY mac", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // rows.Err below reports iteration problems
	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
