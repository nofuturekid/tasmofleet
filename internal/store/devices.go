package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nofuturekid/tasmofleet/internal/model"
)

const deviceCols = `id, name, address, port, mac, topic, full_topic, tags, notes, http_user, http_password,
	ota_excluded, mqtt_capability, chip_family, hardware, single_core, fw_version, fw_image, fw_variant,
	fw_language, flash_size_kb, program_flash_kb, free_kb, identity_at, created_at, updated_at, mqtt_host, mqtt_port,
	net_hostname, net_ip, net_gateway, net_ssid, net_bssid, net_channel, net_wired, net_at, device_name, device_name_kept,
	net_subnet, net_dns, ota_url, relay_names, group_topic, ota_url_read_at, group_topic_read_at,
	mqtt_password, mqtt_password_mode, mqtt_password_bad_at, fw_reach, fw_reach_at, fw_reach_detail, identity_lang_known, fwr_facts, build_memory`

// relayNamesJSON is the stored form of relay names (a JSON array like tags); "" for none.
func relayNamesJSON(names []string) string {
	if len(names) == 0 {
		return ""
	}
	b, err := json.Marshal(names)
	if err != nil { // a []string always marshals
		return ""
	}
	return string(b)
}

type scanner interface{ Scan(dest ...any) error }

func (s *SQLite) scanDevice(r scanner) (model.Device, error) {
	var d model.Device
	var tags, sealed, mqtt, family, relayNames string
	var excluded, single, wired, langKnown int
	var identAt, created, updated, netAt int64
	var otaAt, groupAt sql.NullInt64
	var sealedMQTT, pwMode, reach, fwr string
	var badAt, reachAt sql.NullInt64
	var buildMemory sql.NullString
	err := r.Scan(&d.ID, &d.Name, &d.Address, &d.Port, &d.MAC, &d.Topic, &d.FullTopic, &tags, &d.Notes, &d.HTTPUser, &sealed,
		&excluded, &mqtt, &family, &d.Identity.Hardware, &single, &d.Identity.Version, &d.Identity.Image, &d.Identity.Variant,
		&d.Identity.Language, &d.Identity.FlashSizeKB, &d.Identity.ProgramFlashSizeKB, &d.Identity.FreeKB, &identAt, &created, &updated, &d.MQTTHost, &d.MQTTPort,
		&d.Net.Hostname, &d.Net.IP, &d.Net.Gateway, &d.Net.SSID, &d.Net.BSSID, &d.Net.Channel, &wired, &netAt, &d.DeviceName, &d.KeptDeviceName,
		&d.Net.Subnet, &d.Net.DNS, &d.OTAURL, &relayNames, &d.GroupTopic, &otaAt, &groupAt,
		&sealedMQTT, &pwMode, &badAt, &reach, &reachAt, &d.FWReach.Detail, &langKnown, &fwr, &buildMemory)
	if err != nil {
		return model.Device{}, err
	}
	d.BuildMemory = buildMemory.String
	if err := json.Unmarshal([]byte(tags), &d.Tags); err != nil {
		return model.Device{}, fmt.Errorf("store: device %d tags: %w", d.ID, err)
	}
	if len(d.Tags) == 0 {
		d.Tags = nil
	}
	if relayNames != "" {
		if err := json.Unmarshal([]byte(relayNames), &d.RelayNames); err != nil {
			return model.Device{}, fmt.Errorf("store: device %d relay names: %w", d.ID, err)
		}
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
	if sealedMQTT != "" {
		if d.MQTTPassword, err = s.box.Open(sealedMQTT); err != nil {
			return model.Device{}, fmt.Errorf("store: device %d MQTT password: %w", d.ID, err)
		}
	}
	d.MQTTPasswordMode = model.MQTTPasswordMode(pwMode)
	d.MQTTPasswordBadAt = fromMS(badAt.Int64)
	d.FWReach.State, d.FWReach.At = model.FWReachState(reach), fromMS(reachAt.Int64)
	d.Identity.LanguageKnown = langKnown != 0
	if d.Identity.FWR, err = parseFWR(fwr); err != nil {
		return model.Device{}, fmt.Errorf("store: device %d StatusFWR facts: %w", d.ID, err)
	}
	d.OTAExcluded = excluded != 0
	d.MQTT = model.MQTTCapability(mqtt)
	d.Identity.Family = model.ChipFamily(family)
	d.Identity.SingleCore = single != 0
	d.Identity.DetectedAt = fromMS(identAt)
	d.CreatedAt, d.UpdatedAt = fromMS(created), fromMS(updated)
	d.Net.Wired, d.Net.ReadAt = wired != 0, fromMS(netAt)
	d.OTAURLReadAt, d.GroupTopicReadAt = fromMS(otaAt.Int64), fromMS(groupAt.Int64) // NULL: never read, no age
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
		d.Net.Hostname, d.Net.IP, d.Net.Gateway, d.Net.SSID, d.Net.BSSID, d.Net.Channel, b2i(d.Net.Wired), toMS(d.Net.ReadAt),
		d.Net.Subnet, d.Net.DNS, d.OTAURL, relayNamesJSON(d.RelayNames), d.GroupTopic, nullMS(d.OTAURLReadAt), nullMS(d.GroupTopicReadAt), b2i(d.Identity.LanguageKnown), fwrJSON(d.Identity.FWR))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit; on failure the original error is returned
	res, err := tx.ExecContext(ctx, `INSERT INTO devices (name, address, port, mac, topic, full_topic, tags, notes,
		http_user, http_password, ota_excluded, mqtt_capability, chip_family, hardware, single_core, fw_version, fw_image,
		fw_variant, fw_language, flash_size_kb, program_flash_kb, free_kb, identity_at, created_at, updated_at, mqtt_host, mqtt_port,
		net_hostname, net_ip, net_gateway, net_ssid, net_bssid, net_channel, net_wired, net_at, net_subnet, net_dns, ota_url, relay_names, group_topic, ota_url_read_at, group_topic_read_at, identity_lang_known, fwr_facts)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, args...)
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
		fw_variant=?, fw_language=?, flash_size_kb=?, program_flash_kb=?, free_kb=?, identity_at=?, identity_lang_known=?,
		fwr_facts = CASE WHEN ?='' THEN fwr_facts ELSE ? END, updated_at=? WHERE id=?`,
		string(i.Family), i.Hardware, b2i(i.SingleCore), i.Version, i.Image, i.Variant, i.Language,
		i.FlashSizeKB, i.ProgramFlashSizeKB, i.FreeKB, toMS(i.DetectedAt), b2i(i.LanguageKnown), fwrJSON(i.FWR), fwrJSON(i.FWR), toMS(s.now()), id))
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
	// SetOTAURL is false when the probe did not read Status 1 (minimal firmware, a failed or
	// unusable reply): the stored OTAURL is then kept. OTAURL has no credentials in it.
	SetOTAURL bool
	OTAURL    string
	// SetGroupTopic is false when Status 1 gave no GroupTopic: the stored one and its stamp are kept.
	SetGroupTopic bool
	GroupTopic    string
	// RelayNames are the FriendlyNames the probe read; empty (not read) keeps the stored ones.
	RelayNames []string
	// DeviceName is the device's Tasmota DeviceName, "" when the probe did not read it. It is
	// recorded, and the stored name follows it while the name is still the DeviceName recorded at
	// the previous probe, or NameFrom (#91). Any other name was chosen by the user and is kept.
	DeviceName string
	// NameFrom, when not empty, is a stored name the prober recognised as one tasmofleet filled in
	// itself (hostname, topic or address) before the DeviceName was recorded. It is replaced by
	// DeviceName only if the stored name is still exactly this.
	NameFrom string
	// DeviceTopic is the resolved topic the device reported, "" when not read. It is recorded, and
	// the stored topic always follows it (the stored topic is only a mirror for matching MQTT
	// messages, #111), unless another device already has that topic: then the stored one is kept.
	DeviceTopic string
	// BindMACs binds these MACs (primary first) to a device that has none (guard MAC ""), as an
	// admin's refresh does (#65). A MAC another device holds: ErrMACInUse, nothing written.
	BindMACs []string
}

// followNameSQL is the part of an UPDATE that records the DeviceName a probe read and lets the
// stored name follow it. Seven arguments: DeviceName (name test), DeviceName (the name equals it
// already), NameFrom twice, DeviceName (new name), DeviceName twice (record it). The name follows
// only while it is still the DeviceName recorded at the previous probe, or NameFrom; any other
// name was chosen by the user and stays (the device page offers the DeviceName instead, #125).
const followNameSQL = `name = CASE WHEN ?='' OR name=? THEN name
			WHEN (device_name<>'' AND name=device_name) OR (?<>'' AND name=?) THEN ? ELSE name END,
		device_name = CASE WHEN ?='' THEN device_name ELSE ? END,`

// ErrMACInUse is returned when a probe would bind a MAC that belongs to another device.
var ErrMACInUse = fmt.Errorf("%w: the MAC belongs to another device", ErrConflict)

// UpdateProbe stores the outcome of a probe without touching any inventory
// field the user owns (address, credentials, OTA exclusion, notes, a name or
// topic the user chose, ...), so an edit made during the slow probe is never
// overwritten. If the device was deleted, moved to another address/port or
// rebound to another MAC since the probe began, nothing is written and
// ErrConflict is returned. A device that already reports to our broker keeps
// "ours" when the probe only finds "other" (messages seen beat name matching).
func (s *SQLite) UpdateProbe(ctx context.Context, id model.DeviceID, g ProbeGuard, r ProbeResult) error {
	i := r.Identity
	relayNames := relayNamesJSON(r.RelayNames)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit; on failure the original error is returned
	res, err := tx.ExecContext(ctx, `UPDATE devices SET chip_family=?, hardware=?, single_core=?, fw_version=?, fw_image=?,
		fw_variant=?, fw_language=?, flash_size_kb=?, program_flash_kb=?, free_kb=?, identity_at=?, identity_lang_known=?,
		fwr_facts = CASE WHEN ?='' THEN fwr_facts ELSE ? END,
		mqtt_capability = CASE WHEN ?=0 THEN mqtt_capability WHEN mqtt_capability='ours' AND ?='other' THEN 'ours' ELSE ? END,
		mqtt_host = CASE WHEN ?=0 THEN mqtt_host ELSE ? END,
		mqtt_port = CASE WHEN ?=0 THEN mqtt_port ELSE ? END,
		net_hostname = CASE WHEN ?=0 THEN net_hostname ELSE ? END,
		net_ip = CASE WHEN ?=0 THEN net_ip ELSE ? END,
		net_gateway = CASE WHEN ?=0 THEN net_gateway ELSE ? END,
		net_ssid = CASE WHEN ?=0 THEN net_ssid ELSE ? END,
		net_bssid = CASE WHEN ?=0 THEN net_bssid ELSE ? END,
		net_channel = CASE WHEN ?=0 THEN net_channel ELSE ? END,
		net_wired = CASE WHEN ?=0 THEN net_wired ELSE ? END,
		net_at = CASE WHEN ?=0 THEN net_at ELSE ? END,
		net_subnet = CASE WHEN ?=0 THEN net_subnet ELSE ? END,
		net_dns = CASE WHEN ?=0 THEN net_dns ELSE ? END,
		ota_url = CASE WHEN ?=0 THEN ota_url ELSE ? END,
		ota_url_read_at = CASE WHEN ?=0 THEN ota_url_read_at ELSE ? END,
		group_topic = CASE WHEN ?=0 THEN group_topic ELSE ? END,
		group_topic_read_at = CASE WHEN ?=0 THEN group_topic_read_at ELSE ? END,
		relay_names = CASE WHEN ?='' THEN relay_names ELSE ? END,
		`+followNameSQL+`
		topic = CASE WHEN ?='' OR topic=? THEN topic
			WHEN NOT EXISTS (SELECT 1 FROM devices o WHERE o.topic=? AND o.id<>devices.id) THEN ?
			ELSE topic END,
		device_topic = CASE WHEN ?='' THEN device_topic ELSE ? END,
		updated_at=? WHERE id=? AND address=? AND port=? AND mac=?`,
		string(i.Family), i.Hardware, b2i(i.SingleCore), i.Version, i.Image, i.Variant, i.Language,
		i.FlashSizeKB, i.ProgramFlashSizeKB, i.FreeKB, toMS(i.DetectedAt), b2i(i.LanguageKnown), fwrJSON(i.FWR), fwrJSON(i.FWR),
		b2i(r.SetMQTT), string(r.MQTT), string(r.MQTT),
		b2i(r.SetMQTT), r.MQTTHost, b2i(r.SetMQTT), r.MQTTPort,
		b2i(r.SetNet), r.Net.Hostname, b2i(r.SetNet), r.Net.IP, b2i(r.SetNet), r.Net.Gateway, b2i(r.SetNet), r.Net.SSID,
		b2i(r.SetNet), r.Net.BSSID, b2i(r.SetNet), r.Net.Channel, b2i(r.SetNet), b2i(r.Net.Wired), b2i(r.SetNet), toMS(r.Net.ReadAt),
		b2i(r.SetNet), r.Net.Subnet, b2i(r.SetNet), r.Net.DNS, b2i(r.SetOTAURL), r.OTAURL, b2i(r.SetOTAURL), toMS(s.now()),
		b2i(r.SetGroupTopic), r.GroupTopic, b2i(r.SetGroupTopic), toMS(s.now()), relayNames, relayNames,
		r.DeviceName, r.DeviceName, r.NameFrom, r.NameFrom, r.DeviceName, r.DeviceName, r.DeviceName,
		r.DeviceTopic, r.DeviceTopic, r.DeviceTopic, r.DeviceTopic, r.DeviceTopic, r.DeviceTopic,
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
	if macs := macSet("", r.BindMACs); len(macs) > 0 {
		if g.MAC != "" {
			return fmt.Errorf("%w: device %d already has a MAC, a probe does not rebind it", ErrConflict, id)
		}
		for _, m := range macs {
			var owner model.DeviceID
			switch err := tx.QueryRowContext(ctx, "SELECT device_id FROM device_macs WHERE mac=?", m).Scan(&owner); {
			case err == nil && owner != id:
				return fmt.Errorf("%w: %s, device %d", ErrMACInUse, m, owner)
			case err != nil && !errors.Is(err, sql.ErrNoRows):
				return err
			}
			if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO device_macs (mac, device_id) VALUES (?,?)", m, id); err != nil {
				return mapErr(err)
			}
		}
		if _, err := tx.ExecContext(ctx, "UPDATE devices SET mac=? WHERE id=? AND mac=''", macs[0], id); err != nil {
			return mapErr(err)
		}
	}
	return tx.Commit()
}

// UpdateProbeName stores the DeviceName a probe read, and lets the stored name follow it under the
// rule of UpdateProbe, without touching anything else: a refresh over MQTT reads the DeviceName
// but must not write the identity (#125). Guarded like UpdateProbe: a device deleted, moved or
// rebound since the read began is left alone (ErrConflict). An empty deviceName changes nothing.
func (s *SQLite) UpdateProbeName(ctx context.Context, id model.DeviceID, g ProbeGuard, deviceName, nameFrom string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE devices SET `+followNameSQL+` updated_at=?
		WHERE id=? AND address=? AND port=? AND mac=?`,
		deviceName, deviceName, nameFrom, nameFrom, deviceName, deviceName, deviceName,
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

// DismissDeviceName records that the admin chose to keep the stored name against deviceName, the
// DeviceName the page showed. It only applies while the device still reports exactly that name
// (ErrConflict otherwise: a newer name must not be silenced unseen); ErrNotFound for an unknown device.
func (s *SQLite) DismissDeviceName(ctx context.Context, id model.DeviceID, deviceName string) error {
	res, err := s.db.ExecContext(ctx, "UPDATE devices SET device_name_kept=? WHERE id=? AND device_name=? AND device_name<>''", deviceName, id, deviceName)
	if err != nil {
		return mapErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		var one int
		switch err := s.db.QueryRowContext(ctx, "SELECT 1 FROM devices WHERE id=?", id).Scan(&one); {
		case errors.Is(err, sql.ErrNoRows):
			return ErrNotFound
		case err != nil:
			return err
		}
		return fmt.Errorf("%w: device %d reports another name now", ErrConflict, id)
	}
	return nil
}

// UpdateIdentityFromMQTT stores the identity fields a device announced over MQTT (INFO1, or the
// Status 2 reply asked for over MQTT): chip family, hardware, firmware version, image and
// variant, and nothing else. It writes only while no probe over HTTP has ever identified the
// device (identity_at = 0) and only for the device that was read (address, port, MAC and MQTT
// topic unchanged). A probe that stored an identity meanwhile therefore always wins:
// ErrConflict, nothing written. It never sets identity_at, so the first HTTP probe still runs.
func (s *SQLite) UpdateIdentityFromMQTT(ctx context.Context, id model.DeviceID, g ProbeGuard, topic string, i model.Identity) error {
	res, err := s.db.ExecContext(ctx, `UPDATE devices SET chip_family=?, hardware=?, single_core=?, fw_version=?, fw_image=?,
		fw_variant=?, fwr_facts = CASE WHEN ?='' THEN fwr_facts ELSE ? END, updated_at=? WHERE id=? AND address=? AND port=? AND mac=? AND topic=? AND identity_at=0`,
		string(i.Family), i.Hardware, b2i(i.SingleCore), i.Version, i.Image, i.Variant, fwrJSON(i.FWR), fwrJSON(i.FWR),
		toMS(s.now()), id, g.Address, g.Port, strings.ToLower(g.MAC), topic)
	if err != nil {
		return mapErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: device %d changed, was deleted or was identified over HTTP", ErrConflict, id)
	}
	return nil
}

// UpdateFWRFacts stores the StatusFWR facts of a Status 2 reading and nothing else (#417). Devices
// identified over HTTP before the facts existed (0.22.0) have identity_at set and empty facts; the
// identity writers cannot fill them (UpdateProbe rewrites the identity, UpdateIdentityFromMQTT refuses
// once identity_at is set, which protects the identity). It writes only while the facts are empty (a
// probe's facts are at least as new and win) and only for an identified device; unidentified devices
// get their facts with their identity. Guarded like UpdateProbe: a device deleted, moved or rebound
// since the read began is left alone. Empty facts change nothing. ErrConflict: nothing written.
func (s *SQLite) UpdateFWRFacts(ctx context.Context, id model.DeviceID, g ProbeGuard, f model.FWRFacts) error {
	if !f.Read() {
		return fmt.Errorf("%w: no facts to store for device %d", ErrConflict, id)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE devices SET fwr_facts=?, updated_at=?
		WHERE id=? AND address=? AND port=? AND mac=? AND identity_at<>0 AND fwr_facts=''`,
		fwrJSON(f), toMS(s.now()), id, g.Address, g.Port, strings.ToLower(g.MAC))
	if err != nil {
		return mapErr(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: device %d changed, was deleted, is not identified or already has its facts", ErrConflict, id)
	}
	return nil
}

// UpdateGroupTopicFromMQTT stores the GroupTopic a device announced in INFO1 and the time it was
// read, and nothing else, under the same guard as UpdateProbe plus the topic the message came
// from (#239). It does not care whether HTTP identified the device: the announcement is newer
// than the last probe or at least as good.
func (s *SQLite) UpdateGroupTopicFromMQTT(ctx context.Context, id model.DeviceID, g ProbeGuard, topic, groupTopic string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE devices SET group_topic=?, group_topic_read_at=?
		WHERE id=? AND address=? AND port=? AND mac=? AND topic=?`,
		groupTopic, toMS(s.now()), id, g.Address, g.Port, strings.ToLower(g.MAC), topic)
	if err != nil {
		return mapErr(err)
	}
	if k, err := res.RowsAffected(); err != nil {
		return err
	} else if k == 0 {
		return fmt.Errorf("%w: device %d changed or was deleted", ErrConflict, id)
	}
	return nil
}

// UpdateNet stores the network facts of a background read (app.Prober.ReadNet) and nothing
// else, under the same guard as UpdateProbe: a device deleted, moved or rebound since the read
// began is left alone (ErrConflict).
func (s *SQLite) UpdateNet(ctx context.Context, id model.DeviceID, g ProbeGuard, n model.NetFacts) error {
	res, err := s.db.ExecContext(ctx, `UPDATE devices SET net_hostname=?, net_ip=?, net_gateway=?, net_ssid=?, net_bssid=?,
		net_channel=?, net_wired=?, net_at=?, net_subnet=?, net_dns=? WHERE id=? AND address=? AND port=? AND mac=?`,
		n.Hostname, n.IP, n.Gateway, n.SSID, n.BSSID, n.Channel, b2i(n.Wired), toMS(n.ReadAt), n.Subnet, n.DNS, id, g.Address, g.Port, strings.ToLower(g.MAC))
	if err != nil {
		return mapErr(err)
	}
	if k, err := res.RowsAffected(); err != nil {
		return err
	} else if k == 0 {
		return fmt.Errorf("%w: device %d changed or was deleted during the read", ErrConflict, id)
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

// SetMQTTPassword stores how the device's MqttPassword is found (D6) and clears the bad mark. In
// mode own, plain is sealed like the web password and must not be empty; in the other modes plain
// is ignored and any stored value is removed, so no stale secret stays at rest.
func (s *SQLite) SetMQTTPassword(ctx context.Context, id model.DeviceID, mode model.MQTTPasswordMode, plain string) error {
	sealed := ""
	switch mode {
	case model.MQTTPasswordNone, model.MQTTPasswordBroker:
	case model.MQTTPasswordOwn:
		if plain == "" {
			return errors.New("store: an own MQTT password must not be empty")
		}
		var err error
		if sealed, err = s.box.Seal(plain); err != nil {
			return err
		}
	default:
		return fmt.Errorf("store: unknown MQTT password mode %q", mode)
	}
	return affected(s.db.ExecContext(ctx, `UPDATE devices SET mqtt_password=?, mqtt_password_mode=?, mqtt_password_bad_at=NULL, updated_at=? WHERE id=?`,
		sealed, string(mode), toMS(s.now()), id))
}

// MarkMQTTPasswordBad records that the device refused the password ("Error 1") at at.
func (s *SQLite) MarkMQTTPasswordBad(ctx context.Context, id model.DeviceID, at time.Time) error {
	if at.IsZero() {
		return errors.New("store: the bad mark needs a time")
	}
	return affected(s.db.ExecContext(ctx, `UPDATE devices SET mqtt_password_bad_at=?, updated_at=? WHERE id=?`,
		nullMS(at), toMS(s.now()), id))
}

// SetFWReach records whether the device reaches the firmware server; the zero FWReach resets it
// to unknown ("Forget").
func (s *SQLite) SetFWReach(ctx context.Context, id model.DeviceID, r model.FWReach) error {
	switch r.State {
	case model.FWReachUnknown, model.FWReachYes, model.FWReachNo:
	default:
		return fmt.Errorf("store: unknown firmware reach state %q", r.State)
	}
	return affected(s.db.ExecContext(ctx, `UPDATE devices SET fw_reach=?, fw_reach_at=?, fw_reach_detail=?, updated_at=? WHERE id=?`,
		string(r.State), nullMS(r.At), r.Detail, toMS(s.now()), id))
}

// SetBuildMemory remembers the chip build a person chose for the device ("s2" or "s2cdc"); "" forgets it.
// Only that explicit choice calls this: probes and UpdateDevice never touch the column.
func (s *SQLite) SetBuildMemory(ctx context.Context, id model.DeviceID, build string) error {
	var v any
	switch build {
	case "":
	case "s2", "s2cdc":
		v = build
	default:
		return fmt.Errorf("store: unknown chip build %q", build)
	}
	return affected(s.db.ExecContext(ctx, `UPDATE devices SET build_memory=?, updated_at=? WHERE id=?`, v, toMS(s.now()), id))
}

// fwrJSON is the stored form of the StatusFWR facts: "" when none were read.
func fwrJSON(f model.FWRFacts) string {
	if !f.Read() {
		return ""
	}
	b, err := json.Marshal(f)
	if err != nil { // a struct of strings and one int cannot fail to marshal
		return ""
	}
	return string(b)
}

func parseFWR(raw string) (model.FWRFacts, error) {
	var f model.FWRFacts
	if raw == "" {
		return f, nil
	}
	return f, json.Unmarshal([]byte(raw), &f)
}
