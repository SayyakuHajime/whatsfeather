#include "voicenoterecorder.h"
#include <QDir>
#include <QFileInfo>
#include <QJsonDocument>
#include <QJsonObject>
#include <QJsonArray>
#include <QProcess>
#include <QSignalSpy>
#include <QStandardPaths>
#include <QtTest>
#include <cmath>
#include <memory>
#include <numbers>

namespace {
struct CaptureState {
  bool started = false;
  bool stopped = false;
  bool failStart = false;
};

class SyntheticCaptureSource final : public VoiceCaptureSource {
public:
  explicit SyntheticCaptureSource(std::shared_ptr<CaptureState> state)
      : m_state(std::move(state)) {}

  bool start(QIODevice *destination, PcmFormat *format,
             QString *error) override {
    m_state->started = true;
    if (m_state->failStart) {
      if (error)
        *error = QStringLiteral("synthetic input failure");
      return false;
    }
    if (!destination || !destination->isWritable() || !format) {
      if (error)
        *error = QStringLiteral("capture output unavailable");
      return false;
    }
    format->ffmpegInputFormat = QStringLiteral("s16le");
    format->sampleRate = 48000;
    format->channels = 1;
    format->bytesPerSample = 2;

    QByteArray pcm;
    pcm.reserve(48000 * 2);
    for (int frame = 0; frame < 48000; ++frame) {
      const auto phase = 2.0 * std::numbers::pi * 440.0 * frame / 48000.0;
      const auto sample = static_cast<qint16>(std::sin(phase) * 3000.0);
      pcm.append(static_cast<char>(sample & 0xff));
      pcm.append(static_cast<char>((static_cast<quint16>(sample) >> 8) & 0xff));
    }
    qsizetype written = 0;
    while (written < pcm.size()) {
      const auto count = destination->write(pcm.constData() + written,
                                            pcm.size() - written);
      if (count <= 0) {
        if (error)
          *error = QStringLiteral("synthetic PCM write failed");
        return false;
      }
      written += count;
    }
    return true;
  }

  void stop() override { m_state->stopped = true; }

private:
  std::shared_ptr<CaptureState> m_state;
};

class FailingCaptureSource final : public VoiceCaptureSource {
public:
  bool start(QIODevice *, PcmFormat *, QString *error) override {
    if (error)
      *error = QStringLiteral("synthetic input failure");
    return false;
  }
  void stop() override {}
};

QFileDevice::Permissions forbiddenPermissions() {
  return QFileDevice::ReadGroup | QFileDevice::WriteGroup |
         QFileDevice::ExeGroup | QFileDevice::ReadOther |
         QFileDevice::WriteOther | QFileDevice::ExeOther;
}
} // namespace

class VoiceNoteRecorderTest final : public QObject {
  Q_OBJECT
private slots:
  void encodesSyntheticPcmAsPrivateOggOpusAndDiscardsCleanly() {
    if (QStandardPaths::findExecutable(QStringLiteral("ffmpeg")).isEmpty() ||
        QStandardPaths::findExecutable(QStringLiteral("ffprobe")).isEmpty())
      QSKIP("ffmpeg and ffprobe are required for the voice-note encoding boundary");

    auto state = std::make_shared<CaptureState>();
    VoiceNoteRecorder recorder([state] {
      return std::make_unique<SyntheticCaptureSource>(state);
    });
    QSignalSpy ready(&recorder, &VoiceNoteRecorder::ready);
    QSignalSpy failed(&recorder, &VoiceNoteRecorder::failed);

    QVERIFY(recorder.startRecording());
    QVERIFY(recorder.recording());
    QVERIFY(state->started);
    QVERIFY(!recorder.workingDirectory().isEmpty());
    recorder.stopRecording();
    QVERIFY(!recorder.recording());
    QVERIFY(recorder.encoding());

    QTRY_COMPARE_WITH_TIMEOUT(ready.count(), 1, 20000);
    QCOMPARE(failed.count(), 0);
    QVERIFY(state->stopped);
    const auto outputPath = ready.takeFirst().at(0).toString();
    QCOMPARE(recorder.outputPath(), outputPath);
    const QFileInfo outputInfo(outputPath);
    QVERIFY(outputInfo.isFile());
    QVERIFY(outputInfo.size() > 0);
    QVERIFY(outputInfo.size() <= 20 * 1024 * 1024);
    QVERIFY((outputInfo.permissions() & forbiddenPermissions()) == 0);
    const QFileInfo directoryInfo(outputInfo.absolutePath());
    QVERIFY(directoryInfo.isDir());
    QVERIFY((directoryInfo.permissions() & forbiddenPermissions()) == 0);
    QCOMPARE(QDir(outputInfo.absolutePath()).entryList({QStringLiteral("*.pcm")},
                                                       QDir::Files),
             QStringList{});

    QProcess probe;
    probe.start(QStandardPaths::findExecutable(QStringLiteral("ffprobe")),
                {QStringLiteral("-v"), QStringLiteral("error"),
                 QStringLiteral("-show_entries"),
                 QStringLiteral("stream=codec_name,sample_rate,channels:format=format_name,duration"),
                 QStringLiteral("-of"), QStringLiteral("json"), outputPath});
    QVERIFY(probe.waitForFinished(15000));
    QCOMPARE(probe.exitCode(), 0);
    const auto metadata = QJsonDocument::fromJson(probe.readAllStandardOutput()).object();
    const auto streams = metadata.value(QStringLiteral("streams")).toArray();
    QCOMPARE(streams.size(), 1);
    const auto stream = streams.at(0).toObject();
    QCOMPARE(stream.value(QStringLiteral("codec_name")).toString(),
             QStringLiteral("opus"));
    QCOMPARE(stream.value(QStringLiteral("sample_rate")).toString(),
             QStringLiteral("48000"));
    QCOMPARE(stream.value(QStringLiteral("channels")).toInt(), 1);
    QVERIFY(metadata.value(QStringLiteral("format")).toObject()
                .value(QStringLiteral("format_name")).toString().contains(
                    QStringLiteral("ogg")));

    const auto directory = recorder.workingDirectory();
    recorder.discard();
    QVERIFY(!QFileInfo::exists(outputPath));
    QVERIFY(!QFileInfo::exists(directory));
  }

  void cancelStopsCaptureAndRemovesPrivateWorkspace() {
    auto state = std::make_shared<CaptureState>();
    VoiceNoteRecorder recorder([state] {
      return std::make_unique<SyntheticCaptureSource>(state);
    });
    QSignalSpy ready(&recorder, &VoiceNoteRecorder::ready);
    QVERIFY(recorder.startRecording());
    const auto directory = recorder.workingDirectory();
    QVERIFY(QFileInfo::exists(directory));

    recorder.cancel();

    QVERIFY(!recorder.recording());
    QVERIFY(!recorder.encoding());
    QVERIFY(state->stopped);
    QVERIFY(!QFileInfo::exists(directory));
    QCOMPARE(ready.count(), 0);
  }

  void captureStartupFailureLeavesNoPrivateAudioFiles() {
    VoiceNoteRecorder recorder([] {
      return std::make_unique<FailingCaptureSource>();
    });

    QVERIFY(!recorder.startRecording());
    QVERIFY(!recorder.recording());
    QVERIFY(!recorder.encoding());
    QCOMPARE(recorder.error(), QStringLiteral("synthetic input failure"));
    QVERIFY(recorder.workingDirectory().isEmpty());
    QVERIFY(recorder.outputPath().isEmpty());
  }
};

QTEST_MAIN(VoiceNoteRecorderTest)
#include "test_voicenoterecorder.moc"
