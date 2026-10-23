package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

const deviceCols = `id, name, address, port, mac, topic, full_topic, tags, notes, http_user, http_password,
	ota_excluded, mqtt_capability, chip_family, hardware, single_core, fw_version, fw_image, fw_variant,
	fw_language, flash_size_kb, program_flash_kb, free_kb, identity_at, created_at, updated_at, mqtt_host, mqtt_port,
	net_hostname, net_ip, net_gateway, net_ssid, net_bssid, net_channel, net_at`

type scanner interface{ Scan(dest ...any) error }

func (s *SQLite) scanDevice(r scanner) (model.Device, error) {
	var d model.Device
	var tags, sealed, mqtt, family string
	var excluded, single int
	var identAt, created, updated, netAt int64
	err := r.Scan(&d.ID, &d.Name, &d.Address, &d.Port, &d.MAC, &d.Topic, &d.FullTopic, &tags, &d.Notes, &d.HTTPUser, &sealed,
		&excluded, &mqtt, &family, &d.Identity.Hardware, &single, &d.Identity.Version, &d.Identity.Image, &d.Identity.Variant,
		&d.Identity.Language, &d.Identity.FlashSizeKB, &d.Identity.ProgramFlashSizeKB, &d.Identity.FreeKB, &identAt, &created, &updated, &d.MQTTHost, &d.MQTTPort,
		&d.Net.Hostname, &d.Net.IP, &d.Net.Gateway, &d.Net.SSID, &d.Net.BSSID, &d.Net.Channel, &netAt)
	if err != nil {
		return model.Device{}, err
	}
	if err := json.Unmarshal([]byte(tags), &d.Tags); err != nil {
		return model.Device{}, fmt.Errorf("store: device %d tags: %w", d.ID, err)
	}
	if len(d.Tags) == 0 {
		d.Tags = nil
	}
	if sealed != "" {
		// Fail loud: an empty password would turn a key problem into
		// misleading "auth failed" health states.
		if d.HTTPPassword, err = s.box.Open(sealed); err != nil {
			return model.Device{}, fmt.Errorf("store: device %d password: %w", d.ID, err)
		}
	}
	if d.HTTPPassword == "" {
		// A web user without a password means no auth (the TasmoAdmin import
		// stored a user for every device). Nothing is deleted from the
		// database; the user is simply not part of the loaded device.
		d.HTTPUser = ""
	}
	d.OTAExcluded = excluded != 0
	d.MQTT = model.MQTTCapability(mqtt)
	d.Identity.Family = model.ChipFamily(family)
	d.Identity.SingleCore = single != 0
	d.Identity.DetectedAt = fromMS(identAt)
	d.CreatedAt, d.UpdatedAt = fromMS(created), fromMS(updated)
	d.Net.ReadAt = fromMS(netAt)
	return d, nil
}

func (s *SQLite) getDevice(ctx context.Context, where string, arg any) (model.Device, error) {
	d, err := s.scanDevice(s.db.QueryRowContext(ctx, "SELECT "+deviceCols+" FROM devices WHERE "+where, arg))
	if err != nil {
		return d, notFound(err)
	}
	macs, err := s.deviceMACs(ctx, d.ID)
	if err != nil {
		return model.Device{}, err
	}
	d.MACs = macs[d.ID]
	return d, nil
}

// deviceMACs returns the MAC sets by device (only one device if id != 0),
// each sorted, primary included.
func (s *SQLite) deviceMACs(ctx context.Context, id model.DeviceID) (map[model.DeviceID][]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT device_id, mac FROM device_macs WHERE ?1 = 0 OR device_id = ?1 ORDER BY mac", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // rows.Err below reports iteration problems
	out := map[model.DeviceID][]string{}
	for rows.Next() {
		var id model.DeviceID
		var mac string
		if err := rows.Scan(&id, &mac); err != nil {
			return nil, err
		}
		out[id] = append(out[id], mac)
	}
	return out, rows.Err()
}

// macSet returns primary plus extra, lower case, without empties and duplicates.
func macSet(primary string, extra []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range append([]string{primary}, extra...) {
		if m = strings.ToLower(m); m != "" && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

// ListDevices returns all devices ordered by name, then id.
func (s *SQLite) ListDevices(ctx context.Context) ([]model.Device, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+deviceCols+" FROM devices ORDER BY name COLLATE NOCASE, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // rows.Err below reports iteration problems
	var out []model.Device
	for rows.Next() {
		d, err := s.scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	macs, err := s.deviceMACs(ctx, 0)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].MACs = macs[out[i].ID]
	}
	return out, nil
}

func (s *SQLite) GetDevice(ctx context.Context, id model.DeviceID) (model.Device, error) {
	return s.getDevice(ctx, "id = ?", id)
}

// GetDeviceByMAC matches case-insensitively against the whole MAC set of
// every device; an empty MAC never matches (devices without a known MAC
// store ”).
func (s *SQLite) GetDeviceByMAC(ctx context.Context, mac string) (model.Device, error) {
	if mac == "" {
		return model.Device{}, ErrNotFound
	}
	return s.getDevice(ctx, "id = (SELECT device_id FROM device_macs WHERE mac = ?)", strings.ToLower(mac))
}

// GetDeviceByTopic: an empty topic never matches.
func (s *SQLite) GetDeviceByTopic(ctx context.Context, topic string) (model.Device, error) {
	if topic == "" {
		return model.Device{}, ErrNotFound
	}
	return s.getDevice(ctx, "topic = ?", topic)
}

func (s *SQLite) inventoryArgs(d model.Device) ([]any, error) {
	tags := d.Tags
	if tags == nil {
		tags = []string{}
	}
	tj, err := json.Marshal(tags)
	if err != nil {
		return nil, err
	}
	sealed := ""
	if d.HTTPPassword != "" {
		if sealed, err = s.box.Seal(d.HTTPPassword); err != nil {
			return nil, err
		}
	}
	return []any{d.Name, d.Address, d.Port, strings.ToLower(d.MAC), d.Topic, d.FullTopic, string(tj), d.Notes,
		d.HTTPUser, sealed, b2i(d.OTAExcluded), string(d.MQTT)}, nil
}

// CreateDevice inserts d (ID is ignored). Duplicate MAC, topic or
// address+port yield ErrConflict.
func (s *SQLite) CreateDevice(ctx context.Context, d model.Device) (model.DeviceID, error) {
	args, err := s.inventoryArgs(d)
	if err != nil {
		return 0, err
	}
	now := s.now()
	created := toMS(s.orNow(d.CreatedAt))
	args = append(args,
		string(d.Identity.Family), d.Identity.Hardware, b2i(d.Identity.SingleCore), d.Identity.Version, d.Identity.Image,
		d.Identity.Variant, d.Identity.Language, d.Identity.FlashSizeKB, d.Identity.ProgramFlashSizeKB, d.Identity.FreeKB,
		toMS(d.Identity.DetectedAt), created, toMS(now), d.MQTTHost, d.MQTTPort,
		d.Net.Hostname, d.Net.IP, d.Net.Gateway, d.Net.SSID, d.Net.BSSID, d.Net.Channel, toMS(d.Net.ReadAt))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit; on failure the original error is returned
	res, err := tx.ExecContext(ctx, `INSERT INTO devices (name, address, port, mac, topic, full_topic, tags, notes,
		http_user, http_password, ota_excluded, mqtt_capability, chip_family, hardware, single_core, fw_version, fw_image,
		fw_variant, fw_language, flash_size_kb, program_flash_kb, free_kb, identity_at, created_at, updated_at, mqtt_host, mqtt_port,
		net_hostname, net_ip, net_gateway, net_ssid, net_bssid, net_channel, net_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, args...)
	if err != nil {
		return 0, mapErr(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	for _, m := range macSet(d.MAC, d.MACs) {
		if _, err := tx.ExecContext(ctx, "INSERT INTO device_macs (mac, device_id) VALUES (?,?)", m, id); err != nil {
			return 0, mapErr(err)
		}
	}
	return model.DeviceID(id), tx.Commit()
}

// UpdateDevice writes the inventory fields (name, address, port, MAC, topic,
// tags, notes, credentials, OTAExcluded, MQTT capability and broker). The MAC
// set is kept: only when the primary MAC itself changes (an admin edit) is the
// old primary replaced by the new one in the set; d.MACs is ignored here. The detected
// identity and the health summary are not touched: they have their own
// writers (UpdateIdentity, SetLastSeen), and an inventory edit holding a
// stale copy must not roll them back.
func (s *SQLite) UpdateDevice(ctx context.Context, d model.Device) error {
	args, err := s.inventoryArgs(d)
	if err != nil {
		return err
	}
	args = append(args, d.MQTTHost, d.MQTTPort, toMS(s.now()), d.ID)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit; on failure the original error is returned
	var oldMAC string
	if err := tx.QueryRowContext(ctx, "SELECT mac FROM devices WHERE id=?", d.ID).Scan(&oldMAC); err != nil {
		return notFound(err)
	}
	if err := affected(tx.ExecContext(ctx, `UPDATE devices SET name=?, address=?, port=?, mac=?, topic=?, full_topic=?, tags=?,
		notes=?, http_user=?, http_password=?, ota_excluded=?, mqtt_capability=?, mqtt_host=?, mqtt_port=?, updated_at=? WHERE id=?`, args...)); err != nil {
		return mapErr(err)
	}
	if newMAC := strings.ToLower(d.MAC); newMAC != oldMAC {
		if oldMAC != "" {
			if _, err := tx.ExecContext(ctx, "DELETE FROM device_macs WHERE mac=? AND device_id=?", oldMAC, d.ID); err != nil {
				return err
			}
		}
		if newMAC != "" {
			if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO device_macs (mac, device_id) VALUES (?,?)", newMAC, d.ID); err != nil {
				return mapErr(err)
			}
			var owner model.DeviceID
			if err := tx.QueryRowContext(ctx, "SELECT device_id FROM device_macs WHERE mac=?", newMAC).Scan(&owner); err != nil {
				return err
			}
			if owner != d.ID {
				return fmt.Errorf("%w: MAC %s belongs to device %d", ErrConflict, newMAC, owner)
			}
		}
	}
	return tx.Commit()
}

func (s *SQLite) UpdateIdentity(ctx context.Context, id model.DeviceID, i model.Identity) error {
	return affected(s.db.ExecContext(ctx, `UPDATE devices SET chip_family=?, hardware=?, single_core=?, fw_version=?, fw_image=?,
		fw_variant=?, fw_language=?, flash_size_kb=?, program_flash_kb=?, free_kb=?, identity_at=?, updated_at=? WHERE id=?`,
		string(i.Family), i.Hardware, b2i(i.SingleCore), i.Version, i.Image, i.Variant, i.Language,
		i.FlashSizeKB, i.ProgramFlashSizeKB, i.FreeKB, toMS(i.DetectedAt), toMS(s.now()), id))
}

// ProbeGuard names the record a probe was made against. The result is only
// stored if the device still has this address, port and MAC.
type ProbeGuard struct {
	Address string
	Port    int
	MAC     string
}

// ProbeResult holds the fields a probe owns. SetMQTT is false when the probe
// could not read the MQTT settings (minimal firmware): they are then kept.
type ProbeResult struct {
	Identity model.Identity
	SetMQTT  bool
	MQTT     model.MQTTCapability
	MQTTHost string
	MQTTPort int
	// SetNet is false when the probe read no network facts: the stored ones are kept.
	SetNet bool
	Net    model.NetFacts
}

// UpdateProbe stores the outcome of a probe without touching any inventory
// field (name, address, credentials, OTA exclusion, ...), in one statement,
// so an edit made during the slow probe is never overwritten. If the device
// was deleted, moved to another address/port or rebound to another MAC since
// the probe began, nothing is written and ErrConflict is returned. A device
// that already reports to our broker keeps "ours" when the probe only finds
// "other" (messages seen beat name matching).
func (s *SQLite) UpdateProbe(ctx context.Context, id model.DeviceID, g ProbeGuard, r ProbeResult) error {
	i := r.Identity
	res, err := s.db.ExecContext(ctx, `UPDATE devices SET chip_family=?, hardware=?, single_core=?, fw_version=?, fw_image=?,
		fw_variant=?, fw_language=?, flash_size_kb=?, program_flash_kb=?, free_kb=?, identity_at=?,
		mqtt_capability = CASE WHEN ?=0 THEN mqtt_capability WHEN mqtt_capability='ours' AND ?='other' THEN 'ours' ELSE ? END,
		mqtt_host = CASE WHEN ?=0 THEN mqtt_host ELSE ? END,
		mqtt_port = CASE WHEN ?=0 THEN mqtt_port ELSE ? END,
		net_hostname = CASE WHEN ?=0 THEN net_hostname ELSE ? END,
		net_ip = CASE WHEN ?=0 THEN net_ip ELSE ? END,
		net_gateway = CASE WHEN ?=0 THEN net_gateway ELSE ? END,
		net_ssid = CASE WHEN ?=0 THEN net_ssid ELSE ? END,
		net_bssid = CASE WHEN ?=0 THEN net_bssid ELSE ? END,
		net_channel = CASE WHEN ?=0 THEN net_channel ELSE ? END,
		net_at = CASE WHEN ?=0 THEN net_at ELSE ? END,
		updated_at=? WHERE id=? AND address=? AND port=? AND mac=?`,
		string(i.Family), i.Hardware, b2i(i.SingleCore), i.Version, i.Image, i.Variant, i.Language,
		i.FlashSizeKB, i.ProgramFlashSizeKB, i.FreeKB, toMS(i.DetectedAt),
		b2i(r.SetMQTT), string(r.MQTT), string(r.MQTT),
		b2i(r.SetMQTT), r.MQTTHost, b2i(r.SetMQTT), r.MQTTPort,
		b2i(r.SetNet), r.Net.Hostname, b2i(r.SetNet), r.Net.IP, b2i(r.SetNet), r.Net.Gateway, b2i(r.SetNet), r.Net.SSID,
		b2i(r.SetNet), r.Net.BSSID, b2i(r.SetNet), r.Net.Channel, b2i(r.SetNet), toMS(r.Net.ReadAt),
		toMS(s.now()), id, g.Address, g.Port, strings.ToLower(g.MAC))
	if err != nil {
		return mapErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: device %d changed or was deleted during the probe", ErrConflict, id)
	}
	return nil
}

func (s *SQLite) DeleteDevice(ctx context.Context, id model.DeviceID) error {
	return affected(s.db.ExecContext(ctx, "DELETE FROM devices WHERE id=?", id))
}

func (s *SQLite) SetLastSeen(ctx context.Context, id model.DeviceID, state model.HealthState, at time.Time) error {
	return affected(s.db.ExecContext(ctx, "UPDATE devices SET health_state=?, last_seen_at=? WHERE id=?", string(state), toMS(at), id))
}

// DataVersion is SQLite's PRAGMA data_version: it changes whenever another
// connection or process commits to the database (this connection's own writes
// do not change it). The health tracker polls it to notice inventory changes
// made by the CLI while the server runs. The value is only comparable on one
// connection; openDB pins the pool to exactly one (SetMaxOpenConns(1), no
// lifetime), so consecutive calls always see the same connection. A lost
// connection would at worst cause one spurious reload.
func (s *SQLite) DataVersion(ctx context.Context) (int64, error) {
	var v int64
	err := s.db.QueryRowContext(ctx, "PRAGMA data_version").Scan(&v)
	return v, err
}
