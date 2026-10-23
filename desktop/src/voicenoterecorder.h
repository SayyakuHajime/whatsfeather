#pragma once

#include <QElapsedTimer>
#include <QFile>
#include <QProcess>
#include <QTemporaryDir>
#include <QTemporaryFile>
#include <QTimer>
#include <QObject>
#include <functional>
#include <memory>

class QIODevice;

class VoiceCaptureSource : public QObject {
  Q_OBJECT
public:
  struct PcmFormat {
    QString ffmpegInputFormat;
    int sampleRate = 0;
    int channels = 0;
    int bytesPerSample = 0;
  };

  using QObject::QObject;
  ~VoiceCaptureSource() override = default;

  virtual bool start(QIODevice *destination, PcmFormat *format,
                     QString *error) = 0;
  virtual void stop() = 0;

signals:
  void failed(const QString &message);
};

class VoiceNoteRecorder final : public QObject {
  Q_OBJECT
  Q_PROPERTY(bool recording READ recording NOTIFY stateChanged)
  Q_PROPERTY(bool encoding READ encoding NOTIFY stateChanged)
  Q_PROPERTY(int elapsedSeconds READ elapsedSeconds NOTIFY stateChanged)
  Q_PROPERTY(QString error READ error NOTIFY stateChanged)

public:
  using CaptureFactory =
      std::function<std::unique_ptr<VoiceCaptureSource>()>;

  explicit VoiceNoteRecorder(CaptureFactory factory = {},
                             QObject *parent = nullptr);
  ~VoiceNoteRecorder() override;

  bool recording() const { return m_recording; }
  bool encoding() const { return m_encoding; }
  int elapsedSeconds() const;
  QString error() const { return m_error; }
  QString outputPath() const { return m_ready ? m_outputPath : QString{}; }
  QString workingDirectory() const;

  bool startRecording();
  void stopRecording();
  void cancel();
  void discard();

signals:
  void stateChanged();
  void ready(const QString &path);
  void failed(const QString &message);

private:
  static constexpr int MaxRecordingSeconds = 10 * 60;
  static constexpr qint64 MaxRawBytes = 256LL * 1024 * 1024;
  static constexpr qint64 MaxEncodedBytes = 20LL * 1024 * 1024;

  void updateElapsed();
  void startEncoding();
  void encoderFinished(int exitCode, QProcess::ExitStatus exitStatus);
  void fail(const QString &message);
  void clearWorkspace();
  bool validFormat(const VoiceCaptureSource::PcmFormat &format) const;

  CaptureFactory m_factory;
  std::unique_ptr<VoiceCaptureSource> m_capture;
  std::unique_ptr<QTemporaryDir> m_workingDirectory;
  std::unique_ptr<QTemporaryFile> m_pcmFile;
  QProcess m_encoder;
  QTimer m_elapsedTimer;
  QTimer m_encoderTimeout;
  QElapsedTimer m_clock;
  VoiceCaptureSource::PcmFormat m_format;
  QString m_error;
  QString m_rawPath;
  QString m_outputPath;
  int m_elapsedSeconds = 0;
  bool m_recording = false;
  bool m_encoding = false;
  bool m_ready = false;
};
