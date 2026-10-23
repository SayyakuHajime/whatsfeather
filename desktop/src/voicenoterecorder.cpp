#include "voicenoterecorder.h"

#include <QAudioDevice>
#include <QAudioFormat>
#include <QAudioSource>
#include <QDir>
#include <QFileInfo>
#include <QMediaDevices>
#include <QSet>
#include <QStandardPaths>
#include <QSysInfo>

namespace {
constexpr auto PrivateFilePermissions = QFileDevice::ReadOwner |
                                        QFileDevice::WriteOwner;
constexpr auto PrivateDirectoryPermissions = QFileDevice::ReadOwner |
                                             QFileDevice::WriteOwner |
                                             QFileDevice::ExeOwner;
constexpr auto ForbiddenPermissions = QFileDevice::ReadGroup |
                                      QFileDevice::WriteGroup |
                                      QFileDevice::ExeGroup |
                                      QFileDevice::ReadOther |
                                      QFileDevice::WriteOther |
                                      QFileDevice::ExeOther;

QString ffmpegPcmFormat(const QAudioFormat &format) {
  const bool littleEndian = QSysInfo::ByteOrder == QSysInfo::LittleEndian;
  switch (format.sampleFormat()) {
  case QAudioFormat::UInt8:
    return QStringLiteral("u8");
  case QAudioFormat::Int16:
    return littleEndian ? QStringLiteral("s16le") : QStringLiteral("s16be");
  case QAudioFormat::Int32:
    return littleEndian ? QStringLiteral("s32le") : QStringLiteral("s32be");
  case QAudioFormat::Float:
    return littleEndian ? QStringLiteral("f32le") : QStringLiteral("f32be");
  case QAudioFormat::Unknown:
  case QAudioFormat::NSampleFormats:
    return {};
  }
  return {};
}

class SystemAudioCaptureSource final : public VoiceCaptureSource {
public:
  using VoiceCaptureSource::VoiceCaptureSource;

  bool start(QIODevice *destination, PcmFormat *pcm, QString *error) override {
    if (!destination || !destination->isWritable() || !pcm) {
      if (error)
        *error = QStringLiteral("Private recording output is unavailable.");
      return false;
    }
    const auto device = QMediaDevices::defaultAudioInput();
    if (device.isNull()) {
      if (error)
        *error = QStringLiteral("No microphone input device is available.");
      return false;
    }

    QAudioFormat format;
    format.setSampleRate(48000);
    format.setChannelCount(1);
    format.setSampleFormat(QAudioFormat::Int16);
    if (!device.isFormatSupported(format))
      format = device.preferredFormat();

    const auto inputFormat = ffmpegPcmFormat(format);
    if (!format.isValid() || inputFormat.isEmpty() ||
        format.sampleRate() < 8000 || format.sampleRate() > 96000 ||
        format.channelCount() < 1 || format.channelCount() > 2 ||
        format.bytesPerSample() <= 0) {
      if (error)
        *error = QStringLiteral("The microphone format is not supported for voice notes.");
      return false;
    }

    m_source = std::make_unique<QAudioSource>(device, format);
    const auto bufferBytes = format.bytesForDuration(200000);
    if (bufferBytes > 0)
      m_source->setBufferSize(bufferBytes);
    connect(m_source.get(), &QAudioSource::stateChanged, this,
            [this](QAudio::State state) {
              if (state == QAudio::StoppedState && m_source &&
                  m_source->error() != QtAudio::NoError)
                emit failed(QStringLiteral("Microphone capture stopped unexpectedly."));
            });
    m_source->start(destination);
    if (m_source->error() != QtAudio::NoError) {
      if (error)
        *error = QStringLiteral("Microphone is unavailable or permission was denied.");
      stop();
      return false;
    }
    pcm->ffmpegInputFormat = inputFormat;
    pcm->sampleRate = format.sampleRate();
    pcm->channels = format.channelCount();
    pcm->bytesPerSample = format.bytesPerSample();
    return true;
  }

  void stop() override {
    if (m_source)
      m_source->stop();
    m_source.reset();
  }

private:
  std::unique_ptr<QAudioSource> m_source;
};

std::unique_ptr<VoiceCaptureSource> makeSystemCaptureSource() {
  return std::make_unique<SystemAudioCaptureSource>();
}
} // namespace

VoiceNoteRecorder::VoiceNoteRecorder(CaptureFactory factory, QObject *parent)
    : QObject(parent), m_factory(factory ? std::move(factory)
                                          : CaptureFactory(makeSystemCaptureSource)) {
  m_elapsedTimer.setInterval(250);
  connect(&m_elapsedTimer, &QTimer::timeout, this,
          &VoiceNoteRecorder::updateElapsed);
  m_encoderTimeout.setSingleShot(true);
  connect(&m_encoderTimeout, &QTimer::timeout, this, [this] {
    if (!m_encoding)
      return;
    if (m_encoder.state() != QProcess::NotRunning) {
      m_encoder.kill();
      m_encoder.waitForFinished(1000);
    }
    fail(QStringLiteral("Voice-note encoding exceeded its time limit."));
  });
  connect(&m_encoder,
          qOverload<int, QProcess::ExitStatus>(&QProcess::finished), this,
          &VoiceNoteRecorder::encoderFinished);
  connect(&m_encoder, &QProcess::errorOccurred, this,
          [this](QProcess::ProcessError error) {
            if (m_encoding &&
                (error == QProcess::FailedToStart || error == QProcess::Crashed))
              fail(QStringLiteral("Voice-note encoding could not be completed."));
          });
}

VoiceNoteRecorder::~VoiceNoteRecorder() { cancel(); }

int VoiceNoteRecorder::elapsedSeconds() const {
  if (m_recording && m_clock.isValid())
    return qBound(0, static_cast<int>(m_clock.elapsed() / 1000),
                  MaxRecordingSeconds);
  return m_elapsedSeconds;
}

QString VoiceNoteRecorder::workingDirectory() const {
  return m_workingDirectory ? m_workingDirectory->path() : QString{};
}

bool VoiceNoteRecorder::validFormat(
    const VoiceCaptureSource::PcmFormat &format) const {
  static const QSet<QString> allowed{QStringLiteral("u8"),
                                     QStringLiteral("s16le"),
                                     QStringLiteral("s16be"),
                                     QStringLiteral("s32le"),
                                     QStringLiteral("s32be"),
                                     QStringLiteral("f32le"),
                                     QStringLiteral("f32be")};
  int expectedBytes = 0;
  if (format.ffmpegInputFormat == QStringLiteral("u8"))
    expectedBytes = 1;
  else if (format.ffmpegInputFormat.startsWith(QStringLiteral("s16")))
    expectedBytes = 2;
  else if (format.ffmpegInputFormat.startsWith(QStringLiteral("s32")) ||
           format.ffmpegInputFormat.startsWith(QStringLiteral("f32")))
    expectedBytes = 4;
  return allowed.contains(format.ffmpegInputFormat) &&
         format.sampleRate >= 8000 && format.sampleRate <= 96000 &&
         format.channels >= 1 && format.channels <= 2 &&
         format.bytesPerSample == expectedBytes;
}

bool VoiceNoteRecorder::startRecording() {
  if (m_recording || m_encoding)
    return false;
  if (QStandardPaths::findExecutable(QStringLiteral("ffmpeg")).isEmpty()) {
    m_error = QStringLiteral("Voice-note encoding is unavailable because ffmpeg is missing.");
    emit stateChanged();
    emit failed(m_error);
    return false;
  }

  cancel();
  m_error.clear();
  m_workingDirectory = std::make_unique<QTemporaryDir>(
      QDir::tempPath() + QStringLiteral("/whatsfeather-voice-XXXXXX"));
  if (!m_workingDirectory->isValid()) {
    fail(QStringLiteral("A private voice-note workspace could not be created."));
    return false;
  }
  if (!QFile::setPermissions(m_workingDirectory->path(),
                             PrivateDirectoryPermissions)) {
    fail(QStringLiteral("The voice-note workspace could not be made private."));
    return false;
  }
  m_pcmFile = std::make_unique<QTemporaryFile>(
      m_workingDirectory->filePath(QStringLiteral("capture-XXXXXX.pcm")));
  m_pcmFile->setAutoRemove(true);
  if (!m_pcmFile->open() ||
      !m_pcmFile->setPermissions(PrivateFilePermissions)) {
    fail(QStringLiteral("A private voice-note capture file could not be created."));
    return false;
  }

  m_capture = m_factory ? m_factory() : nullptr;
  if (!m_capture) {
    fail(QStringLiteral("Microphone capture is unavailable."));
    return false;
  }
  connect(m_capture.get(), &VoiceCaptureSource::failed, this,
          [this](const QString &message) {
            if (m_recording)
              fail(message.isEmpty()
                       ? QStringLiteral("Microphone capture failed.")
                       : message);
          });

  m_recording = true;
  m_clock.start();
  m_elapsedSeconds = 0;
  QString captureError;
  if (!m_capture->start(m_pcmFile.get(), &m_format, &captureError)) {
    if (m_recording)
      fail(captureError.isEmpty()
               ? QStringLiteral("Microphone capture could not be started.")
               : captureError);
    return false;
  }
  if (!m_recording)
    return false;
  if (!validFormat(m_format)) {
    fail(QStringLiteral("The microphone returned an unsupported audio format."));
    return false;
  }
  m_elapsedTimer.start();
  emit stateChanged();
  return true;
}

void VoiceNoteRecorder::updateElapsed() {
  if (!m_recording)
    return;
  m_elapsedSeconds = elapsedSeconds();
  emit stateChanged();
  if (m_elapsedSeconds >= MaxRecordingSeconds)
    stopRecording();
}

void VoiceNoteRecorder::stopRecording() {
  if (!m_recording)
    return;
  m_elapsedTimer.stop();
  m_recording = false;
  if (m_capture) {
    disconnect(m_capture.get(), nullptr, this, nullptr);
    m_capture->stop();
    m_capture.reset();
  }
  if (!m_pcmFile || !m_pcmFile->isOpen() || !m_pcmFile->flush()) {
    fail(QStringLiteral("The voice-note recording could not be saved."));
    return;
  }
  const auto rawSize = m_pcmFile->size();
  m_rawPath = m_pcmFile->fileName();
  m_pcmFile->close();
  if (rawSize <= 0 || rawSize > MaxRawBytes ||
      m_format.bytesPerSample <= 0 || m_format.channels <= 0 ||
      m_format.sampleRate <= 0) {
    fail(QStringLiteral("The voice-note recording is empty or too large."));
    return;
  }
  const auto bytesPerFrame =
      static_cast<qint64>(m_format.bytesPerSample) * m_format.channels;
  if (rawSize % bytesPerFrame != 0) {
    fail(QStringLiteral("The voice-note recording ended on an invalid audio frame."));
    return;
  }
  const auto durationSeconds =
      static_cast<double>(rawSize) /
      (static_cast<double>(m_format.sampleRate) * bytesPerFrame);
  if (durationSeconds < 0.5 || durationSeconds > MaxRecordingSeconds) {
    fail(QStringLiteral("Record at least half a second and no more than ten minutes."));
    return;
  }
  m_elapsedSeconds = qMax(m_elapsedSeconds,
                           static_cast<int>(std::ceil(durationSeconds)));
  m_outputPath = m_workingDirectory->filePath(QStringLiteral("voice-note.ogg"));
  m_encoding = true;
  emit stateChanged();
  startEncoding();
}

void VoiceNoteRecorder::startEncoding() {
  const auto ffmpeg = QStandardPaths::findExecutable(QStringLiteral("ffmpeg"));
  if (ffmpeg.isEmpty()) {
    fail(QStringLiteral("Voice-note encoding is unavailable because ffmpeg is missing."));
    return;
  }
  const QStringList arguments{
      QStringLiteral("-hide_banner"), QStringLiteral("-loglevel"),
      QStringLiteral("error"), QStringLiteral("-nostdin"),
      QStringLiteral("-y"), QStringLiteral("-f"),
      m_format.ffmpegInputFormat, QStringLiteral("-ar"),
      QString::number(m_format.sampleRate), QStringLiteral("-ac"),
      QString::number(m_format.channels), QStringLiteral("-i"), m_rawPath,
      QStringLiteral("-map"), QStringLiteral("0:a:0"), QStringLiteral("-vn"),
      QStringLiteral("-c:a"), QStringLiteral("libopus"),
      QStringLiteral("-application"), QStringLiteral("voip"),
      QStringLiteral("-b:a"), QStringLiteral("24k"),
      QStringLiteral("-vbr"), QStringLiteral("constrained"),
      QStringLiteral("-ar"), QStringLiteral("48000"), QStringLiteral("-ac"),
      QStringLiteral("1"), QStringLiteral("-t"),
      QString::number(MaxRecordingSeconds), QStringLiteral("-f"),
      QStringLiteral("ogg"), m_outputPath};
  m_encoder.setProgram(ffmpeg);
  m_encoder.setArguments(arguments);
  m_encoder.setProcessChannelMode(QProcess::SeparateChannels);
  m_encoderTimeout.start(20000);
  m_encoder.start();
}

void VoiceNoteRecorder::encoderFinished(int exitCode,
                                        QProcess::ExitStatus exitStatus) {
  if (!m_encoding)
    return;
  m_encoderTimeout.stop();
  if (exitStatus != QProcess::NormalExit || exitCode != 0) {
    fail(QStringLiteral("Voice-note encoding failed."));
    return;
  }
  QFile output(m_outputPath);
  if (!output.open(QIODevice::ReadOnly)) {
    fail(QStringLiteral("The encoded voice note could not be opened."));
    return;
  }
  const auto size = output.size();
  const auto signature = output.read(4);
  output.close();
  if (size <= 0 || size > MaxEncodedBytes || signature != QByteArrayLiteral("OggS")) {
    fail(QStringLiteral("The encoded voice note is invalid or exceeds 20 MiB."));
    return;
  }
  if (!QFile::setPermissions(m_outputPath, PrivateFilePermissions)) {
    fail(QStringLiteral("The encoded voice note could not be made private."));
    return;
  }
  m_pcmFile.reset();
  m_rawPath.clear();
  m_encoding = false;
  m_ready = true;
  m_error.clear();
  emit stateChanged();
  emit ready(m_outputPath);
}

void VoiceNoteRecorder::fail(const QString &message) {
  m_elapsedTimer.stop();
  m_encoderTimeout.stop();
  m_recording = false;
  m_encoding = false;
  m_ready = false;
  if (m_capture) {
    disconnect(m_capture.get(), nullptr, this, nullptr);
    m_capture->stop();
    m_capture.reset();
  }
  if (m_encoder.state() != QProcess::NotRunning) {
    m_encoder.kill();
    m_encoder.waitForFinished(1000);
  }
  clearWorkspace();
  m_elapsedSeconds = 0;
  m_error = message;
  emit stateChanged();
  emit failed(m_error);
}

void VoiceNoteRecorder::clearWorkspace() {
  if (m_pcmFile) {
    if (m_pcmFile->isOpen())
      m_pcmFile->close();
    m_pcmFile.reset();
  }
  m_rawPath.clear();
  m_outputPath.clear();
  if (m_workingDirectory) {
    m_workingDirectory->remove();
    m_workingDirectory.reset();
  }
}

void VoiceNoteRecorder::cancel() {
  m_elapsedTimer.stop();
  m_encoderTimeout.stop();
  m_recording = false;
  m_encoding = false;
  m_ready = false;
  if (m_capture) {
    disconnect(m_capture.get(), nullptr, this, nullptr);
    m_capture->stop();
    m_capture.reset();
  }
  if (m_encoder.state() != QProcess::NotRunning) {
    m_encoder.terminate();
    if (!m_encoder.waitForFinished(500)) {
      m_encoder.kill();
      m_encoder.waitForFinished(1000);
    }
  }
  clearWorkspace();
  m_elapsedSeconds = 0;
  m_error.clear();
  emit stateChanged();
}

void VoiceNoteRecorder::discard() { cancel(); }
