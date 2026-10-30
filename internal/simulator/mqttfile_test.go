package simulator

import (
	"bytes"
	"crypto/md5" //nolint:gosec // the protocol mandates MD5
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"
)

const testMQTTPassword = "fake-mqtt-password"

// fileBus talks to one device over the embedded broker the way TasmoFleet's
// MQTT client will: commands on cmnd/<topic>/<command>, answers on stat.
type fileBus struct {
	t     *testing.T
	b     *Broker
	topic string
	down  chan string
	up    chan string
}

func newFileBus(t *testing.T, b *Broker, topic string) *fileBus {
	t.Helper()
	fb := &fileBus{t: t, b: b, topic: topic, down: make(chan string, 64), up: make(chan string, 64)}
	for suffix, ch := range map[string]chan string{"FILEDOWNLOAD": fb.down, "FILEUPLOAD": fb.up} {
		if err := b.Subscribe("stat/"+topic+"/"+suffix, func(_ string, p []byte, _ bool) { ch <- string(p) }); err != nil {
			t.Fatal(err)
		}
	}
	return fb
}

func (fb *fileBus) send(command, payload string) {
	fb.t.Helper()
	if err := fb.b.Publish("cmnd/"+fb.topic+"/"+command, []byte(payload), false); err != nil {
		fb.t.Fatal(err)
	}
}

func next(t *testing.T, ch chan string) string {
	t.Helper()
	select {
	case m := <-ch:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("no message from the device")
		return ""
	}
}

func silent(t *testing.T, ch chan string) {
	t.Helper()
	select {
	case m := <-ch:
		t.Fatalf("the device must stay silent, got %s", m)
	case <-time.After(250 * time.Millisecond):
	}
}

func jsonField(t *testing.T, msg, key string) any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(msg), &m); err != nil {
		t.Fatalf("%s: %v", msg, err)
	}
	return m[key]
}

// download runs the documented client loop: start, then one "?" per reply
// (the header answers the start; none after "Started").
func (fb *fileBus) download(pw string) (data []byte, md5hex string, chunks []int) {
	fb.t.Helper()
	fb.send("filedownload", fmt.Sprintf(`{"Password":%q,"Type":2}`, pw))
	if got := next(fb.t, fb.down); got != `{"FileDownload":"Started"}` {
		fb.t.Fatalf("first reply: %s", got)
	}
	header := next(fb.t, fb.down)
	size := int(jsonField(fb.t, header, "Size").(float64))
	for {
		fb.send("filedownload", "?")
		m := next(fb.t, fb.down)
		if md := jsonField(fb.t, m, "Md5"); md != nil {
			if got := next(fb.t, fb.down); got != `{"FileDownload":"Done"}` {
				fb.t.Fatalf("after Md5: %s", got)
			}
			if len(data) != size {
				fb.t.Fatalf("got %d bytes of %d", len(data), size)
			}
			return data, md.(string), chunks
		}
		raw, err := base64.StdEncoding.DecodeString(jsonField(fb.t, m, "Data").(string))
		if err != nil {
			fb.t.Fatal(err)
		}
		data = append(data, raw...)
		chunks = append(chunks, len(raw))
	}
}

func brokerDevice(t *testing.T, name string, profile Profile, faults ...Fault) (*Broker, *collector, Controller, *fileBus) {
	t.Helper()
	b, c, f := newBrokerFleet(t, Extra{})
	d := add(t, f, DeviceSpec{Name: name, Profile: profile, Version: "16.0.0", MQTTPassword: testMQTTPassword, Faults: faults})
	c.waitFor(t, "[R]tele/"+name+"/LWT=Online")
	return b, c, d, newFileBus(t, b, name)
}

func dl(t *testing.T, d Device) []byte {
	t.Helper()
	body, code, err := cmWithPath(d, "/dl", "")
	if err != nil || code != 200 {
		t.Fatalf("/dl: %d %v", code, err)
	}
	return []byte(body)
}

// Why: backup over MQTT must give the bytes /dl gives, in the chunking the
// device really uses (live 2026-10-30: 2980 + 1116 B on an ESP8285, because
// ResponseSize is 4000 there); an ESP32 has ResponseSize 6000 and sends 4096 B at once.
func TestFileDownloadIsByteIdenticalToDlAndChunkedLikeTheDevice(t *testing.T) {
	for _, tc := range []struct {
		profile Profile
		chunks  []int
	}{{ProfileESP8266_1M, []int{2980, 1116}}, {ProfileESP32, []int{4096}}} {
		t.Run(string(tc.profile), func(t *testing.T) {
			_, _, d, fb := brokerDevice(t, "dl-"+string(tc.profile), tc.profile)
			data, sum, chunks := fb.download(testMQTTPassword)
			if !bytes.Equal(data, dl(t, d)) {
				t.Error("FileDownload differs from /dl")
			}
			want := md5.Sum(data) //nolint:gosec // see import
			if sum != hex.EncodeToString(want[:]) {
				t.Errorf("device MD5 %s does not match the data", sum)
			}
			if fmt.Sprint(chunks) != fmt.Sprint(tc.chunks) {
				t.Errorf("chunks %v want %v", chunks, tc.chunks)
			}
		})
	}
}

// Why: a "?" the client sends before it has seen a reply makes the device run
// one step ahead (every "?" is a step; live 2026-10-30 such a stray "?" ended in
// 1116 B and an MD5 mismatch). The client must send exactly one per reply and
// abort when unsure; and an abort of a running download first sends one more
// chunk (CmndFileDownload publishes before it validates), so a client has to
// drain it.
func TestFileDownloadStrayQuestionMarkRunsAheadAndAbortSendsOneMoreChunk(t *testing.T) {
	_, _, _, fb := brokerDevice(t, "stray", ProfileESP8266_1M)
	fb.send("filedownload", fmt.Sprintf(`{"Password":%q,"Type":2}`, testMQTTPassword))
	next(t, fb.down) // Started
	next(t, fb.down) // header
	fb.send("filedownload", "?")
	if m := next(t, fb.down); jsonField(t, m, "Data") == nil {
		t.Fatalf("first step is not data: %s", m)
	}
	fb.send("filedownload", "?") // stray: nobody waited for the previous reply
	fb.send("filedownload", "?")
	if m := next(t, fb.down); jsonField(t, m, "Data") == nil {
		t.Fatalf("second step is not data: %s", m)
	}
	if m := next(t, fb.down); jsonField(t, m, "Md5") == nil {
		t.Fatalf("the device is one step ahead and must already send the MD5: %s", m)
	}
	next(t, fb.down) // Done

	fb.send("filedownload", fmt.Sprintf(`{"Password":%q,"Type":2}`, testMQTTPassword))
	next(t, fb.down)
	next(t, fb.down)
	fb.send("filedownload", "0")
	if m := next(t, fb.down); jsonField(t, m, "Data") == nil {
		t.Fatalf("abort must first publish a chunk: %s", m)
	}
	if got := next(t, fb.down); got != `{"FileDownload":"Aborted"}` {
		t.Errorf("abort reply: %s", got)
	}
}

// Why: the password is the device's MqttPassword; a device that answered with
// a wrong one would hide a restore/backup that the real device refuses.
func TestFileCommandsRefuseWrongPasswordAndGroupTopic(t *testing.T) {
	_, _, _, fb := brokerDevice(t, "guarded", ProfileESP8266_4M)
	fb.send("filedownload", `{"Password":"wrong","Type":2}`)
	if got := next(t, fb.down); got != `{"FileDownload":"Error 1"}` {
		t.Errorf("download, wrong password: %s", got)
	}
	fb.send("fileupload", `{"Password":"wrong","File":"x.dmp","Id":116,"Type":2,"Size":4096}`)
	if got := next(t, fb.up); got != `{"FileUpload":"Error 1"}` {
		t.Errorf("upload, wrong password: %s", got)
	}
	if err := fb.b.Publish("cmnd/tasmotas/fileupload", []byte(fmt.Sprintf(`{"Password":%q,"Id":116,"Type":2,"Size":4096}`, testMQTTPassword)), false); err != nil {
		t.Fatal(err)
	}
	if got := next(t, fb.up); got != `{"FileUpload":"Error 5"}` {
		t.Errorf("group topic: %s", got)
	}
}

// uploadAll sends data in chunks of the announced MaxSize and returns the
// final reply. start is the already received reply to the start command.
func (fb *fileBus) uploadAll(id int, data []byte, startArgs string) (maxSize int, final string) {
	fb.t.Helper()
	sum := md5.Sum(data) //nolint:gosec // see import
	fb.send("fileupload", fmt.Sprintf(`{"Password":%q,"File":"x","Id":%d,%s,"Size":%d}`, testMQTTPassword, id, startArgs, len(data)))
	if got := next(fb.t, fb.up); got != `{"FileUpload":"Started"}` {
		fb.t.Fatalf("start: %s", got)
	}
	maxSize = int(jsonField(fb.t, next(fb.t, fb.up), "MaxSize").(float64))
	for off := 0; off < len(data); off += maxSize {
		end := min(off+maxSize, len(data))
		fb.send("fileupload", fmt.Sprintf(`{"Id":%d,"Data":"%s"}`, id, base64.StdEncoding.EncodeToString(data[off:end])))
		if m := next(fb.t, fb.up); jsonField(fb.t, m, "MaxSize") == nil {
			fb.t.Fatalf("chunk ack: %s", m)
		}
	}
	fb.send("fileupload", fmt.Sprintf(`{"Id":%d,"Md5":"%s"}`, id, hex.EncodeToString(sum[:])))
	return maxSize, next(fb.t, fb.up)
}

// Why: restore over MQTT. MaxSize 850 for a topic of 34 characters on the wire
// is what the device announced live (formula from the 1200 B buffer); the dump
// must end up as the device's settings, with the restart that follows Done and
// the LWT Offline that comes with it.
func TestFileUploadType2RestoresTheDumpAndRestarts(t *testing.T) {
	_, c, d, fb := brokerDevice(t, "balcony-windowsill", ProfileESP8266_1M)
	want := bytes.Repeat([]byte{0x5A}, configDumpSize)
	maxSize, final := fb.uploadAll(116, want, `"Type":2`)
	if maxSize != 850 {
		t.Errorf("MaxSize %d, the live device said 850", maxSize)
	}
	if final != `{"FileUpload":"Done"}` {
		t.Fatalf("final reply: %s", final)
	}
	settled(t, d, 1)
	if !bytes.Equal(dl(t, d), want) {
		t.Error("the restored dump is not what /dl returns after the restart")
	}
	c.waitFor(t, "[R]tele/balcony-windowsill/LWT=Offline")
}

// Why: a wrong MD5 must not restore anything or restart the device.
func TestFileUploadMD5MismatchChangesNothing(t *testing.T) {
	_, _, d, fb := brokerDevice(t, "md5", ProfileESP8266_4M)
	before := dl(t, d)
	fb.send("fileupload", fmt.Sprintf(`{"Password":%q,"Id":9,"Type":2,"Size":4096}`, testMQTTPassword))
	next(t, fb.up)
	maxSize := int(jsonField(t, next(t, fb.up), "MaxSize").(float64))
	data := bytes.Repeat([]byte{1}, configDumpSize)
	for off := 0; off < len(data); off += maxSize {
		fb.send("fileupload", fmt.Sprintf(`{"Id":9,"Data":"%s"}`, base64.StdEncoding.EncodeToString(data[off:min(off+maxSize, len(data))])))
		next(t, fb.up)
	}
	fb.send("fileupload", `{"Id":9,"Md5":"00000000000000000000000000000000"}`)
	if got := next(t, fb.up); got != `{"FileUpload":"MD5 mismatch"}` {
		t.Errorf("reply: %s", got)
	}
	time.Sleep(200 * time.Millisecond)
	if !bytes.Equal(dl(t, d), before) || d.Boots() != 0 {
		t.Error("a corrupt upload changed the device")
	}
}

// Why: PubSubClient closes the connection on a packet of 1200 B or more
// (no DISCONNECT, so the will fires). A client that builds a chunk one byte too
// big would otherwise see the simulator accept what the device never does.
func TestOversizedPacketDropsTheConnection(t *testing.T) {
	b, c, _, _ := brokerDevice(t, "overflow", ProfileESP8266_4M)
	topic := "cmnd/overflow/Status"
	// wire size = 1 + 2 (remaining length) + 2 + len(topic) + len(payload)
	fit := 1199 - 5 - len(topic)
	if err := b.Publish(topic, []byte("2"+strings.Repeat(" ", fit-1)), false); err != nil {
		t.Fatal(err)
	}
	c.waitFor(t, "stat/overflow/STATUS2=")
	if c.has("tele/overflow/LWT=Offline") || c.has("[R]tele/overflow/LWT=Offline") {
		t.Fatal("a packet one byte below the limit must be accepted")
	}
	if err := b.Publish(topic, []byte("2"+strings.Repeat(" ", fit)), false); err != nil {
		t.Fatal(err)
	}
	c.waitFor(t, "[R]tele/overflow/LWT=Offline")
}

// Why: Type 1 is only possible where the image fits head_room, and the rule
// must predict both outcomes live saw and the source implies: Error 2 for a
// full image on the safeboot ESP32 (office-monitor-thomas, 2026-10-30), success
// on a dual-partition layout.
func TestFileUploadType1FitsOnlyWhereHeadRoomAllows(t *testing.T) {
	t.Run("safeboot layout refuses a full image before any data", func(t *testing.T) {
		_, _, d, fb := brokerDevice(t, "single", ProfileESP32)
		fb.send("fileupload", fmt.Sprintf(`{"Password":%q,"File":"tasmota32.bin","Id":116,"Type":1,"Size":2234912}`, testMQTTPassword))
		if got := next(t, fb.up); got != `{"FileUpload":"Error 2"}` {
			t.Errorf("got %s", got)
		}
		if d.Boots() != 0 {
			t.Error("the device restarted")
		}
	})
	t.Run("dual layout takes the image and boots the new version", func(t *testing.T) {
		_, c, d, fb := brokerDevice(t, "dual", ProfileESP32Dual)
		img, err := NewImage(fixtures, "tasmota32.bin", "16.1.0")
		if err != nil {
			t.Fatal(err)
		}
		_, final := fb.uploadAll(116, img, `"Type":1`)
		if final != `{"FileUpload":"Done"}` {
			t.Fatalf("final: %s", final)
		}
		settled(t, d, 1)
		eq(t, "version", d.Version(), "16.1.0")
		c.waitFor(t, "[R]tele/dual/LWT=Offline")
	})
	t.Run("dual layout still refuses what does not fit", func(t *testing.T) {
		_, _, _, fb := brokerDevice(t, "dual-big", ProfileESP32Dual)
		fb.send("fileupload", fmt.Sprintf(`{"Password":%q,"Id":116,"Type":1,"Size":2900000}`, testMQTTPassword))
		if got := next(t, fb.up); got != `{"FileUpload":"Error 2"}` {
			t.Errorf("got %s", got)
		}
	})
}

// Why: the bootloader's header check also applies to an image sent over MQTT:
// a wrong-family image must fail without a restart.
func TestFileUploadType1WrongFamilyFailsWithoutRestart(t *testing.T) {
	_, _, d, fb := brokerDevice(t, "wrongfam", ProfileESP32Dual)
	img, err := NewImage(fixtures, "tasmota32c3.bin", "16.1.0")
	if err != nil {
		t.Fatal(err)
	}
	_, final := fb.uploadAll(116, img, `"Type":1`)
	if final != `{"FileUpload":"Failed"}` {
		t.Errorf("final: %s", final)
	}
	time.Sleep(200 * time.Millisecond)
	eq(t, "boots", d.Boots(), 0)
}

// Why: the minimal and safeboot builds compile the file commands out
// (FIRMWARE_MINIMAL), so Type 1 can never be the second stage.
func TestFileCommandsAreUnknownInMinimal(t *testing.T) {
	b, c, _, _ := brokerDevice(t, "mini", ProfileESP8266Min)
	if err := b.Publish("cmnd/mini/fileupload", []byte(`{"Password":"x","Id":1,"Type":1,"Size":4096}`), false); err != nil {
		t.Fatal(err)
	}
	c.waitFor(t, `stat/mini/RESULT={"Command":"Unknown"`)
}

// Why: the payload of a file command carries the device password; the
// recorded commands (Commands(), used in assertions and dev output) must not,
// whichever way the command arrived: MQTT, HTTP /cm or inside a Backlog.
func TestRecordedFileCommandsDoNotContainThePassword(t *testing.T) {
	b, _, d, fb := brokerDevice(t, "redact", ProfileESP8266_4M)
	fb.download(testMQTTPassword)
	payload := fmt.Sprintf(`{"Password":"%s","Type":2}`, testMQTTPassword)
	cm(t, d, "FileDownload "+payload)
	cm(t, d, "FileDownload 0")
	if err := b.Publish("cmnd/redact/Backlog", []byte("Power ON;FileDownload "+payload), false); err != nil {
		t.Fatal(err)
	}
	if err := b.Publish("cmnd/redact/Status", []byte("2"), false); err != nil { // behind the backlog
		t.Fatal(err)
	}
	for i := 0; i < 100 && len(d.Commands()) < 8; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	cmds := d.Commands()
	var sawHTTP, sawBacklog bool
	for _, cmd := range cmds {
		if strings.Contains(cmd, testMQTTPassword) {
			t.Errorf("recorded %q", cmd)
		}
		sawHTTP = sawHTTP || strings.HasPrefix(cmd, "FileDownload <")
		sawBacklog = sawBacklog || strings.HasPrefix(cmd, "Backlog ")
	}
	if !sawHTTP || !sawBacklog {
		t.Errorf("the HTTP and Backlog commands were not recorded: %v", cmds)
	}
}

// Why: a link that dies halfway through an upload leaves the device untouched;
// the engine's per-ack timeout must be what ends the transfer.
func TestFaultMQTTUploadCutStopsAcknowledging(t *testing.T) {
	_, _, d, fb := brokerDevice(t, "cut", ProfileESP32Dual, Fault{Kind: FaultMQTTUploadCut, Chunks: 1})
	img, _ := NewImage(fixtures, "tasmota32.bin", "16.1.0")
	fb.send("fileupload", fmt.Sprintf(`{"Password":%q,"Id":116,"Type":1,"Size":%d}`, testMQTTPassword, len(img)))
	next(t, fb.up)
	per := int(jsonField(t, next(t, fb.up), "MaxSize").(float64))
	chunk := func(i int) {
		fb.send("fileupload", fmt.Sprintf(`{"Id":116,"Data":"%s"}`, base64.StdEncoding.EncodeToString(img[i*per:min((i+1)*per, len(img))])))
	}
	chunk(0)
	next(t, fb.up)
	chunk(1)
	silent(t, fb.up)
	eq(t, "boots", d.Boots(), 0)
}

// Why: the nastiest case for the engine is a flash that worked while the final
// Done never arrived: it must verify the new version, not resend.
func TestFaultMQTTUploadLostDoneStillFlashes(t *testing.T) {
	_, _, d, fb := brokerDevice(t, "lostdone", ProfileESP32Dual, Fault{Kind: FaultMQTTUploadLostDone})
	img, _ := NewImage(fixtures, "tasmota32.bin", "16.1.0")
	fb.send("fileupload", fmt.Sprintf(`{"Password":%q,"Id":116,"Type":1,"Size":%d}`, testMQTTPassword, len(img)))
	next(t, fb.up)
	maxSize := int(jsonField(t, next(t, fb.up), "MaxSize").(float64))
	for off := 0; off < len(img); off += maxSize {
		fb.send("fileupload", fmt.Sprintf(`{"Id":116,"Data":"%s"}`, base64.StdEncoding.EncodeToString(img[off:min(off+maxSize, len(img))])))
		next(t, fb.up)
	}
	sum := md5.Sum(img) //nolint:gosec // see import
	fb.send("fileupload", fmt.Sprintf(`{"Id":116,"Md5":"%s"}`, hex.EncodeToString(sum[:])))
	silent(t, fb.up)
	settled(t, d, 1)
	eq(t, "version", d.Version(), "16.1.0")
}

// Why: a device TasmoFleet cannot reach over HTTP but the broker can is the
// whole case for the MQTT transport. The prober must see a refused connection
// (what an unreachable host gives), not an EOF, and it must stay that way after
// a reboot.
func TestFaultHTTPUnreachableRefusesConnectionsAndKeepsMQTT(t *testing.T) {
	b, c, d, _ := brokerDevice(t, "nohttp", ProfileESP8266_4M, Fault{Kind: FaultHTTPUnreachable})
	refused := func(when string) {
		t.Helper()
		conn, err := net.DialTimeout("tcp", d.Addr(), time.Second)
		if err == nil {
			_ = conn.Close()
			t.Fatalf("%s: the connection was accepted", when)
		}
		if !errors.Is(err, syscall.ECONNREFUSED) {
			t.Errorf("%s: %v, want connection refused", when, err)
		}
	}
	refused("at start")
	if err := b.Publish("cmnd/nohttp/Power", []byte("ON"), false); err != nil {
		t.Fatal(err)
	}
	c.waitFor(t, `stat/nohttp/RESULT={"POWER":"ON"}`)
	if err := b.Publish("cmnd/nohttp/Restart", []byte("1"), false); err != nil {
		t.Fatal(err)
	}
	settled(t, d, 1)
	refused("after a reboot")
}

// Why: with a firmware server the device cannot reach, the restore download is
// a synchronous connect attempt that ends in "Connect failed" (WebGetConfig,
// v16 xdrv_01_9_webserver.ino 4510-4588); an immediate "Done" would send the
// restore flow looking for a reboot that never comes.
func TestFaultFWUnreachableMakesWebGetConfigFailAfterTheConnectTimeout(t *testing.T) {
	f := newTestFleet(t, "", Extra{})
	d := add(t, f, DeviceSpec{Name: "nofwrestore", Profile: ProfileESP8266_4M, Faults: []Fault{{Kind: FaultFWUnreachable, Duration: 150 * time.Millisecond}}})
	start := time.Now()
	got := cm(t, d, "WebGetConfig http://127.0.0.1:1/restore/tok")
	if got != `{"WebGetConfig":"Connect failed"}` {
		t.Errorf("reply: %s", got)
	}
	if time.Since(start) < 150*time.Millisecond {
		t.Error("the failure came before the connect timeout")
	}
	time.Sleep(100 * time.Millisecond)
	eq(t, "boots", d.Boots(), 0)
}

// Why: the failure the maintainer's VLAN produces: Upgrade 1 is accepted, the
// device cannot connect, reports it after the connect timeout and neither
// reboots nor goes silent (live, 2026-10-30).
func TestFaultFWUnreachableReportsConnectionFailedWithoutReboot(t *testing.T) {
	b, c, d, _ := brokerDevice(t, "nofw", ProfileESP8266_4M, Fault{Kind: FaultFWUnreachable, Duration: 150 * time.Millisecond})
	cm(t, d, "OtaUrl http://127.0.0.1:1/fw/16.1.0/tasmota.bin.gz")
	start := time.Now()
	if err := b.Publish("cmnd/nofw/Upgrade", []byte("1"), false); err != nil {
		t.Fatal(err)
	}
	c.waitFor(t, `stat/nofw/UPGRADE={"Upgrade":"Failed HTTP error: connection failed"}`)
	if time.Since(start) < 150*time.Millisecond {
		t.Error("the failure came before the connect timeout")
	}
	eq(t, "boots", d.Boots(), 0)
	eq(t, "phase", d.Phase(), PhaseRun)
}

// Why: Backlog sent over MQTT answers nothing itself; every queued command
// publishes its own RESULT (CmndBacklog calls ResponseClear). A client waiting
// for one merged reply would wait forever.
func TestBacklogOverMQTTPublishesOneResultPerCommand(t *testing.T) {
	b, c, _, _ := brokerDevice(t, "bl", ProfileESP8266_4M)
	if err := b.Publish("cmnd/bl/Backlog", []byte("Power ON;Power OFF"), false); err != nil {
		t.Fatal(err)
	}
	c.waitFor(t, `stat/bl/RESULT={"POWER":"OFF"}`)
	if c.count(`stat/bl/RESULT={"POWER":"ON"}`) != 1 {
		t.Error("each command of the backlog must publish its own RESULT")
	}
	if c.has("stat/bl/RESULT={\"POWER\":\"ON\",") || c.has(`stat/bl/RESULT={"Backlog"`) {
		t.Error("Backlog must not publish a merged or own reply")
	}
}

func device(t *testing.T, c Controller) *simDevice {
	t.Helper()
	d, ok := c.(*simDevice)
	if !ok {
		t.Fatalf("%T is not a simDevice", c)
	}
	return d
}

// Why: head_room decides which devices may take an update over MQTT, so the
// formula (not a constant) has to be pinned per layout. Inputs are the Status 4
// numbers of the profile; the expectations are worked out from the v16 source
// (docs/research/tasmota-mqtt-transport.md section 3):
//   - esp8266-1m: sketch 651 KB, Free 352 KB: ceil4k(sketch) + Free - floor4k(sketch) - 16 KiB = 340 KiB;
//   - esp8266-4m: sketch 651 KB, Free 740 KB: 728 KiB;
//   - esp32 safeboot layout: sketch 2182 KB, Free 697 KB, running partition at 64 KiB:
//     floor64k(end) - floor64k(start + sketch + 128 KiB) = 512 KiB;
//   - esp32 with two app partitions: Free 2880 KB is the other partition:
//     floor64k(partition) - 128 KiB = 2752 KiB.
//
// For each layout an upload of exactly head_room is accepted and one sector more
// is Error 2 (Error 2 comes before any data is touched).
func TestHeadRoomPerLayoutAndTheBoundaryOfAType1Upload(t *testing.T) {
	for _, tc := range []struct {
		profile Profile
		kib     int
	}{{ProfileESP8266_1M, 340}, {ProfileESP8266_4M, 728}, {ProfileESP32, 512}, {ProfileESP32Dual, 2752}} {
		t.Run(string(tc.profile), func(t *testing.T) {
			_, _, c, fb := brokerDevice(t, "room-"+string(tc.profile), tc.profile)
			sd := device(t, c)
			sd.mu.Lock()
			got := sd.headRoomLocked()
			sd.mu.Unlock()
			if got != tc.kib*1024 {
				t.Fatalf("head_room %d KiB, want %d KiB", got/1024, tc.kib)
			}
			fb.send("fileupload", fmt.Sprintf(`{"Password":%q,"Id":116,"Type":1,"Size":%d}`, testMQTTPassword, got))
			if m := next(t, fb.up); m != `{"FileUpload":"Started"}` {
				t.Fatalf("Size == head_room: %s", m)
			}
			next(t, fb.up) // MaxSize
			fb.send("fileupload", "0")
			if m := next(t, fb.up); m != `{"FileUpload":"Aborted"}` {
				t.Fatalf("abort: %s", m)
			}
			fb.send("fileupload", fmt.Sprintf(`{"Password":%q,"Id":117,"Type":1,"Size":%d}`, testMQTTPassword, got+4096))
			if m := next(t, fb.up); m != `{"FileUpload":"Error 2"}` {
				t.Errorf("Size == head_room + one sector: %s", m)
			}
		})
	}
}

// Why: a settings dump may be 2 to 16 KiB (SettingsBufferAlloc); only more is
// Error 2. Refusing a 4 KiB-plus size would keep a valid restore off the MQTT path.
func TestFileUploadType2AcceptsUpTo16KiB(t *testing.T) {
	_, _, _, fb := brokerDevice(t, "t2size", ProfileESP8266_4M)
	for _, size := range []int{2048, 8192, 16384} {
		fb.send("fileupload", fmt.Sprintf(`{"Password":%q,"Id":5,"Type":2,"Size":%d}`, testMQTTPassword, size))
		if m := next(t, fb.up); m != `{"FileUpload":"Started"}` {
			t.Fatalf("Size %d: %s", size, m)
		}
		next(t, fb.up)
		fb.send("fileupload", "0")
		next(t, fb.up)
	}
	fb.send("fileupload", fmt.Sprintf(`{"Password":%q,"Id":5,"Type":2,"Size":16385}`, testMQTTPassword))
	if m := next(t, fb.up); m != `{"FileUpload":"Error 2"}` {
		t.Errorf("Size 16385: %s", m)
	}
}

// Why: every way out of a transfer must be a clear answer the client can act
// on: Aborted for FileUpload 0 and for a data message with another Id, Error 3
// for a type the device does not know (after the room check).
func TestFileUploadAbortMismatchedIdAndInvalidType(t *testing.T) {
	_, _, _, fb := brokerDevice(t, "abort", ProfileESP8266_4M)
	start := func() {
		fb.send("fileupload", fmt.Sprintf(`{"Password":%q,"Id":116,"Type":2,"Size":4096}`, testMQTTPassword))
		next(t, fb.up)
		next(t, fb.up)
	}
	start()
	fb.send("fileupload", "0")
	if m := next(t, fb.up); m != `{"FileUpload":"Aborted"}` {
		t.Errorf("FileUpload 0: %s", m)
	}
	start()
	fb.send("fileupload", `{"Id":117,"Data":"AAAA"}`)
	if m := next(t, fb.up); m != `{"FileUpload":"Aborted"}` {
		t.Errorf("mismatched Id: %s", m)
	}
	fb.send("fileupload", fmt.Sprintf(`{"Password":%q,"Id":118,"Type":3,"Size":4096}`, testMQTTPassword))
	if m := next(t, fb.up); m != `{"FileUpload":"Error 3"}` {
		t.Errorf("type 3: %s", m)
	}
}

// Why: the device converts chunk_size to the base64 payload size again on every
// message that arrives while no byte has been stored (F 282-284). A sender that
// sends an empty Data first sees MaxSize shrink 850 -> 619; sizing chunks from
// the latest MaxSize (not the first) is what the protocol needs.
func TestFileUploadMaxSizeShrinksOnRepeatedPositionZeroMessages(t *testing.T) {
	_, _, _, fb := brokerDevice(t, "balcony-windowsill", ProfileESP8266_4M)
	fb.send("fileupload", fmt.Sprintf(`{"Password":%q,"Id":116,"Type":2,"Size":4096}`, testMQTTPassword))
	next(t, fb.up)
	if got := jsonField(t, next(t, fb.up), "MaxSize"); got != float64(850) {
		t.Fatalf("first MaxSize %v", got)
	}
	fb.send("fileupload", `{"Id":116,"Data":""}`)
	if got := jsonField(t, next(t, fb.up), "MaxSize"); got != float64(619) {
		t.Errorf("MaxSize after an empty message: %v, want 619", got)
	}
}

// Why: the ESP32 safeboot image is built with FIRMWARE_MINIMAL, so it has no
// file commands either; a push over MQTT cannot target it.
func TestFileCommandsAreUnknownInESP32Safeboot(t *testing.T) {
	b, c, f := newBrokerFleet(t, Extra{})
	add(t, f, DeviceSpec{Name: "sb", Profile: ProfileESP32, Variant: "safeboot", MQTTPassword: testMQTTPassword})
	c.waitFor(t, "[R]tele/sb/LWT=Online")
	if err := b.Publish("cmnd/sb/filedownload", []byte(`{"Password":"x","Type":2}`), false); err != nil {
		t.Fatal(err)
	}
	c.waitFor(t, `stat/sb/RESULT={"Command":"Unknown"`)
}

// Why (review #284): "FileUpload 0" runs Update.end(true). An abort after every chunk was sent commits the
// complete image for the next restart; an abort of a partial one leaves the device on its firmware. The
// engine relies on this to never abort a complete Type 1 transfer.
func TestFileUploadAbortOfACompleteType1ImageIsCommittedAtTheNextRestart(t *testing.T) {
	for _, tc := range []struct {
		name    string
		chunks  int // data messages sent before the abort; -1 = all of them
		version string
	}{{"complete", -1, "16.1.0"}, {"partial", 1, ""}} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, d, fb := brokerDevice(t, "abort-"+tc.name, ProfileESP32Dual)
			before := d.Version()
			img, _ := NewImage(fixtures, "tasmota32.bin", "16.1.0")
			fb.send("fileupload", fmt.Sprintf(`{"Password":%q,"Id":116,"Type":1,"Size":%d}`, testMQTTPassword, len(img)))
			next(t, fb.up)
			maxSize := int(jsonField(t, next(t, fb.up), "MaxSize").(float64))
			sent := 0
			for off := 0; off < len(img) && (tc.chunks < 0 || sent < tc.chunks); off += maxSize {
				fb.send("fileupload", fmt.Sprintf(`{"Id":116,"Data":"%s"}`, base64.StdEncoding.EncodeToString(img[off:min(off+maxSize, len(img))])))
				next(t, fb.up)
				sent++
			}
			fb.send("fileupload", "0")
			next(t, fb.up) // Aborted
			eq(t, "version right after the abort", d.Version(), before)
			fb.send("restart", "1")
			settled(t, d, 1)
			want := tc.version
			if want == "" {
				want = before
			}
			eq(t, "version after the restart", d.Version(), want)
		})
	}
}
