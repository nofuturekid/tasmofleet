# Commands over MQTT: threat model and claims

Scope: how tasmofleet sends commands, settings writes and policy fixes to a device over its MQTT
broker instead of its HTTP API (`internal/transport`, `internal/mqttcmd`, spec 6.5 and 6.6).
Backup, restore and update over MQTT extend this document when they land. Findings are judged
against it: valid if they break a claim below or show a residual risk worse than stated.

## What changes compared with HTTP

- On the MQTT path only the broker protects the device (user, ACL, TLS). The device's web password
  plays no part; anyone who can publish to `cmnd/<topic>/#` can do what the console can do.
- Commands, settings writes and their replies travel through the broker. Secret-bearing writes
  (`WebPassword`, `MqttPassword`, `Password<n>`, ...) are allowed, as over HTTP; the console warns when
  the broker URL is not `ssl://`.

## Claims

| #   | Claim                                                                                                                                                                                                                                                                       | Test                                                                                                     |
| --- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------- |
| T1  | `Status 0` is never published, in any spelling, alone or in a `Backlog`, and not by a command that runs other commands: `Json`, `Publish`, `Publish2`, `Rule` with a rule text, `RuleTimer`, `WebSend`, `WebQuery`, `Br`, `Script` are refused by the channel.              | `TestStatus0IsNeverPublished`, `TestCommandsThatRunOtherCommandsAreRefused`, `TestSimStatus0CannotBePublishedByAnExecutor` |
| T2  | A command is sent on the other transport inside one call only when the first provably never left: a refused connection, an address outside DEVICE_NETWORKS, a 401, SetOption128. A timeout, an unparsable answer or a dropped connection is never followed by a second send. | `TestFailsOverToMQTTOnlyWhenNothingWasSent`, `TestRefusedConnectionIsOneMQTTSend`, `TestNotSent`         |
| T3  | A published command is never sent again over HTTP. A channel closed while a command waits reports an unknown outcome, not "not sent"; a closed channel publishes nothing.                                                                                                  | `TestAFailedMQTTSendIsNeverRepeatedOverHTTP`, `TestCloseWakesACommandInFlight`, `TestDoAfterCloseNeverPublishes` |
| T4  | Reads follow the read path, every other command the stricter write path (an unread GroupTopic of this or another device blocks writes). Calls without a device ID never go to MQTT.                                                                                       | `TestReadsAndWritesFollowTheirOwnPath`, `TestTargetWithoutDeviceNeverUsesMQTT`                            |
| T5  | An upgrade or restore command is sent over MQTT only by a job that pinned the path.                                                                                                                                                                                         | `TestUpgradeAndRestoreCommandsOnlyGoOverMQTTPinned`                                                      |
| T6  | The topic is checked again right before the publish; a changed topic, group topic, FullTopic or MQTT capability sends nothing.                                                                                                                                              | `TestTopicChangeBetweenSnapshotAndSendSendsNothing`                                                      |
| T7  | The reply of a credential-setting command is masked before anything shows or stores it. `FileDownload`, `FileUpload` and `Upload` (their payload carries the device's MQTT password) are refused for every role.                                                              | `TestSecretReplyIsScrubbedOverMQTT`, `TestFileTransferCommandsAreNeverSent`                              |
| T8  | A change that can cut off a device reached over MQTT only (Topic, FullTopic, GroupTopic, Mqtt*, `SetOption3 0`) asks for a confirmation that says so.                                                                                                                         | `TestMQTTOnlyNamesTheCutOffConsequence`, `TestConsoleMQTTOnlyDeviceAsksBeforeACutOffChange`              |
| T10 | A write that can cut off a device reached over MQTT only is published only with a person's confirmation (the console's); Make compliant and bulk never carry it. The failure of a write after a refused-API answer is not repeated over MQTT unless the memory already knew the device refuses the API. | `TestACutOffWriteNeedsAConfirmationOverMQTT`, `TestAPIRefusedFailsOverOnlyForReadsOrAKnownRefusal`, `TestApplyLeavesOutWhatCouldCutAnMQTTOnlyDeviceOff` |
| T9  | The audit says which transport carried a write (`via`).                                                                                                                                                                                                                      | `TestRunOverMQTTIsAuditedAndScrubbed`                                                                    |

## Residual risk

- **Low.** A broker client that spoofs a device topic can answer a query (a reply is matched by topic and by
  the command's key, not by a signature). Identity checks (MAC via Status 5, family) still run before restore
  and update.
- **Low.** `GroupTopic2` to `GroupTopic4` are not stored; a device listening on one of them is not seen as
  sharing the topic. A device with a topic that neither HTTP nor our broker reaches keeps an unread
  GroupTopic and holds back MQTT writes for the others.
