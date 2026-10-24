// Private same-UID RPC and native rendering probe for a synthetic poll creation.
#include "controller.h"
#include <QAccessible>
#include <QDir>
#include <QFileDevice>
#include <QFileInfo>
#include <QGuiApplication>
#include <QHash>
#include <QImage>
#include <QJsonArray>
#include <QJsonDocument>
#include <QJsonObject>
#include <QLocalServer>
#include <QLocalSocket>
#include <QQuickItem>
#include <QQuickWindow>
#include <QKeyEvent>
#include <QTemporaryDir>
#include <QTimer>
#include <QDebug>
#include <memory>

namespace {
constexpr int MaxFrame = 256 * 1024;
const QString ChatID = QStringLiteral("fixture:poll-chat");
const QString MessageID = QStringLiteral("fixture:incoming-poll");
const QString ResultsMessageID = QStringLiteral("fixture:incoming-poll-results");
const QString Question = QStringLiteral("<b>Choose & inspect?</b>");
const QString ResultsQuestion = QStringLiteral("Weekend plan?");
const QString ResultsOptionOne = QStringLiteral("Beach");
const QString ResultsOptionTwo = QStringLiteral("Hiking");
const QString OptionOne = QStringLiteral("<script>literal</script>");
const QString OptionTwo = QStringLiteral("A & B");
const QString LegacyBody = QStringLiteral("Old <b>poll</b> https://example.invalid/no-link");

QObject *named(QObject *root, const QString &name) {
  if (!root)
    return nullptr;
  if (auto *window = qobject_cast<QQuickWindow *>(root)) {
    if (auto *visual = named(window->contentItem(), name))
      return visual;
  }
  if (root->objectName() == name)
    return root;
  if (auto *item = qobject_cast<QQuickItem *>(root))
    for (auto *child : item->childItems())
      if (auto *found = named(child, name))
        return found;
  for (auto *child : root->children())
    if (auto *found = named(child, name))
      return found;
  return nullptr;
}

bool keyboardActivate(QObject *object) {
  auto *item = qobject_cast<QQuickItem *>(object);
  if (!item || !item->isVisible() || !item->isEnabled())
    return false;
  item->forceActiveFocus(Qt::TabFocusReason);
  QKeyEvent press(QEvent::KeyPress, Qt::Key_Space, Qt::NoModifier);
  QCoreApplication::sendEvent(item, &press);
  QKeyEvent release(QEvent::KeyRelease, Qt::Key_Space, Qt::NoModifier);
  QCoreApplication::sendEvent(item, &release);
  return true;
}

QObject *bubbleForMessage(QObject *root, const QString &id) {
  if (!root)
    return nullptr;
  if (root->objectName() == QStringLiteral("messageBubble") &&
      root->property("record").toMap().value(QStringLiteral("id")).toString() == id)
    return root;
  if (auto *window = qobject_cast<QQuickWindow *>(root))
    if (auto *found = bubbleForMessage(window->contentItem(), id))
      return found;
  if (auto *item = qobject_cast<QQuickItem *>(root))
    for (auto *child : item->childItems())
      if (auto *found = bubbleForMessage(child, id))
        return found;
  for (auto *child : root->children())
    if (auto *found = bubbleForMessage(child, id))
      return found;
  return nullptr;
}

QObject *namedWithText(QObject *root, const QString &name, const QString &text) {
  if (!root)
    return nullptr;
  if (root->objectName() == name && root->property("text").toString() == text)
    return root;
  if (auto *window = qobject_cast<QQuickWindow *>(root))
    if (auto *visual = namedWithText(window->contentItem(), name, text))
      return visual;
  if (auto *item = qobject_cast<QQuickItem *>(root))
    for (auto *child : item->childItems())
      if (auto *found = namedWithText(child, name, text))
        return found;
  for (auto *child : root->children())
    if (auto *found = namedWithText(child, name, text))
      return found;
  return nullptr;
}

class SyntheticPollRPC final : public QObject {
public:
  explicit SyntheticPollRPC(QObject *parent = nullptr) : QObject(parent) {}
  ~SyntheticPollRPC() override {
    m_server.close();
    for (auto *socket : m_input.keys())
      QObject::disconnect(socket, nullptr, this, nullptr);
  }

  bool start() {
    m_temp = std::make_shared<QTemporaryDir>(QDir::tempPath() + QStringLiteral("/wf-poll-XXXXXX"));
    if (!m_temp->isValid()) {
      m_error = QStringLiteral("cannot create private poll fixture directory");
      return false;
    }
    if (!QFile::setPermissions(m_temp->path(), QFileDevice::ReadOwner | QFileDevice::WriteOwner | QFileDevice::ExeOwner)) {
      m_error = QStringLiteral("cannot secure poll fixture directory");
      return false;
    }
    m_socketPath = m_temp->path() + QStringLiteral("/rpc.sock");
    if (m_socketPath.toLocal8Bit().size() >= 100) {
      m_error = QStringLiteral("private poll fixture socket exceeds the AF_UNIX budget");
      return false;
    }
    m_server.setSocketOptions(QLocalServer::UserAccessOption);
    if (!m_server.listen(m_socketPath)) {
      m_error = m_server.errorString();
      return false;
    }
    const auto forbidden = QFileDevice::ReadGroup | QFileDevice::WriteGroup |
                           QFileDevice::ExeGroup | QFileDevice::ReadOther |
                           QFileDevice::WriteOther | QFileDevice::ExeOther;
    if (QFileInfo(m_socketPath).permissions() & forbidden) {
      m_error = QStringLiteral("poll fixture socket is not private");
      return false;
    }
    connect(&m_server, &QLocalServer::newConnection, this, [this] {
      while (m_server.hasPendingConnections()) {
        auto *socket = m_server.nextPendingConnection();
        m_input.insert(socket, {});
        connect(socket, &QLocalSocket::readyRead, this, [this, socket] {
          auto &buffer = m_input[socket];
          buffer += socket->readAll();
          if (buffer.size() > MaxFrame && !buffer.contains('\n')) {
            socket->abort();
            return;
          }
          while (true) {
            const auto newline = buffer.indexOf('\n');
            if (newline < 0)
              break;
            if (newline + 1 > MaxFrame) {
              socket->abort();
              return;
            }
            const auto frame = buffer.left(newline);
            buffer.remove(0, newline + 1);
            const auto request = QJsonDocument::fromJson(frame).object();
            if (!request.isEmpty())
              handle(socket, request);
          }
        });
        connect(socket, &QLocalSocket::disconnected, this, [this, socket] {
          m_input.remove(socket);
          socket->deleteLater();
        });
      }
    });
    return true;
  }

  QString socketPath() const { return m_socketPath; }
  QString error() const { return m_error; }
  int unexpectedMethodCount() const { return m_unexpectedMethodCount; }
  int voteCount() const { return m_voteCount; }
  bool votingEnabled() const { return qEnvironmentVariableIntValue("WHATSFEATHER_POLL_TEST_VOTING") == 1; }

private:
  void reply(QLocalSocket *socket, const QString &id, const QJsonObject &result) {
    const auto frame = QJsonDocument(QJsonObject{{"v", 1}, {"id", id}, {"result", result}})
                           .toJson(QJsonDocument::Compact) + '\n';
    if (frame.size() <= MaxFrame) {
      socket->write(frame);
      socket->flush();
    }
  }

  void handle(QLocalSocket *socket, const QJsonObject &request) {
    const auto id = request.value(QStringLiteral("id")).toString();
    const auto method = request.value(QStringLiteral("method")).toString();
    if (method == QStringLiteral("app.status")) {
      reply(socket, id, {{"mode", "demo"}, {"state", "demo"}, {"message", "Private synthetic poll fixture"}, {"revision", 1}});
    } else if (method == QStringLiteral("app.capabilities")) {
      QJsonArray methods{"app.status", "app.capabilities", "account.get", "chat.list", "message.list", "draft.get", "draft.set"};
      if (votingEnabled()) methods.append("poll.vote");
      reply(socket, id, {{"version", 1}, {"methods", methods}});
    } else if (method == QStringLiteral("account.get")) {
      reply(socket, id, {{"available", false}, {"state", "demo"}});
    } else if (method == QStringLiteral("chat.list")) {
      reply(socket, id, {{"chats", QJsonArray{QJsonObject{{"id", ChatID}, {"title", "Synthetic poll"}, {"initials", "SP"}, {"timestamp", 1000}}}}, {"has_more", false}, {"revision", 1}});
    } else if (method == QStringLiteral("message.list")) {
      const QJsonObject poll{{"question", Question}, {"options", QJsonArray{OptionOne, OptionTwo}}, {"selectable_options_count", 1}};
      const QJsonObject pollResults{{"question", ResultsQuestion},
                                    {"options", QJsonArray{QJsonObject{{"name", ResultsOptionOne}, {"vote_count", 2}},
                                                           QJsonObject{{"name", ResultsOptionTwo}, {"vote_count", 0}}}}};
      QJsonObject message{{"id", MessageID}, {"chat_id", ChatID}, {"body", Question},
                                {"timestamp", 1000}, {"outgoing", false}, {"status", "received"},
                                {"kind", "poll"}, {"poll", poll}};
      if (!m_pollMessage.isEmpty()) message = m_pollMessage;
      if (!qEnvironmentVariable("WHATSFEATHER_POLL_TEST_BLOCKED").isEmpty())
        message.insert("poll_vote_state", qEnvironmentVariable("WHATSFEATHER_POLL_TEST_BLOCKED"));
      m_pollMessage = message;
      const QJsonObject resultsMessage{{"id", ResultsMessageID}, {"chat_id", ChatID}, {"body", ResultsQuestion},
                                       {"timestamp", 1500}, {"outgoing", false}, {"status", "received"},
                                       {"kind", "poll_results"}, {"poll_results", pollResults}};
      const QJsonObject legacy{{"id", "fixture:legacy-poll"}, {"chat_id", ChatID}, {"body", LegacyBody},
                               {"timestamp", 2000}, {"outgoing", false}, {"status", "received"},
                               {"kind", "poll"}};
      reply(socket, id, {{"messages", QJsonArray{message, resultsMessage, legacy}}, {"has_more", false}});
    } else if (method == QStringLiteral("poll.vote") && votingEnabled()) {
      const auto params = request.value("params").toObject();
      if (params.value("chat_id").toString() != ChatID ||
          params.value("target_id").toString() != MessageID ||
          params.value("option_indices").toArray() != QJsonArray{0} ||
          params.value("client_id").toString().isEmpty() || ++m_voteCount != 1) {
        ++m_unexpectedMethodCount;
        reply(socket, id, {{"invalid_fixture_vote", true}});
        return;
      }
      m_pollMessage.insert("poll_results", QJsonObject{{"question", Question},
          {"options", QJsonArray{QJsonObject{{"name", OptionOne}, {"vote_count", 1}},
                                  QJsonObject{{"name", OptionTwo}, {"vote_count", 0}}}}});
      reply(socket, id, {{"message", m_pollMessage}});
    } else if (method == QStringLiteral("draft.get")) {
      reply(socket, id, {{"text", ""}});
    } else if (method == QStringLiteral("draft.set")) {
      reply(socket, id, {{"saved", true}});
    } else {
      ++m_unexpectedMethodCount;
      reply(socket, id, {{"unsupported", true}});
    }
  }

  std::shared_ptr<QTemporaryDir> m_temp;
  QString m_socketPath, m_error;
  QLocalServer m_server;
  QHash<QLocalSocket *, QByteArray> m_input;
  int m_unexpectedMethodCount = 0;
  int m_voteCount = 0;
  QJsonObject m_pollMessage;
};
} // namespace

QObject *startPollFixture(QGuiApplication &, Controller &controller,
                          const QString &dataDir) {
  auto *fixture = new SyntheticPollRPC(QCoreApplication::instance());
  if (!fixture->start()) {
    qCritical().noquote() << "Cannot start private poll fixture:" << fixture->error();
    delete fixture;
    return nullptr;
  }
  controller.start(fixture->socketPath(), dataDir, false);
  return fixture;
}

void runPollTest(QGuiApplication &app, QQuickWindow *window,
                 Controller &controller, QObject *fixtureObject) {
  auto *fixture = static_cast<SyntheticPollRPC *>(fixtureObject);
  if (!fixture || !window) {
    qCritical() << "Native poll UI FAIL: missing private fixture or window";
    app.exit(1);
    return;
  }
  struct State { int ticks = 0; int stage = 0; };
  auto state = std::make_shared<State>();
  auto *timer = new QTimer(&app);
  timer->setInterval(50);
  QObject::connect(
      timer, &QTimer::timeout, &app,
      [state, timer, window, controllerPtr = &controller, fixture,
       appPtr = &app] {
        auto fail = [timer, appPtr](const QString &message) {
          qCritical().noquote() << "Native poll UI FAIL:" << message;
          timer->stop();
          appPtr->exit(1);
        };
        auto &controller = *controllerPtr;
        if (++state->ticks > 240) {
          fail(QStringLiteral("timed out waiting for synthetic poll card"));
          return;
        }
        if (!controller.connected() || !controller.capabilitiesReady() ||
            controller.selectedId() != ChatID || controller.loading() ||
            controller.messageCount() != 3)
          return;

        if (fixture->votingEnabled()) {
          auto *pollBubble = bubbleForMessage(window, MessageID);
          if (!pollBubble)
            return;
          if (!qEnvironmentVariable("WHATSFEATHER_POLL_TEST_BLOCKED")
                   .isEmpty()) {
            auto *button = named(pollBubble, QStringLiteral("pollVoteButton"));
            if (!button)
              return;
            if (button->property("enabled").toBool() ||
                button->property("text").toString() !=
                    QStringLiteral("Vote pending · check outcome") ||
                fixture->voteCount() != 0 ||
                controller.performMessageAction(
                    "poll.vote", pollBubble->property("record").toMap(),
                    {{"option_indices", QVariantList{0}}})) {
              fail(QStringLiteral("durable unresolved vote must disable the "
                                  "control and reject dispatch"));
              return;
            }
            const auto capturePath =
                qEnvironmentVariable("WHATSFEATHER_POLL_TEST_CAPTURE");
            if (!capturePath.isEmpty() &&
                !window->grabWindow().save(capturePath)) {
              fail(QStringLiteral("blocked vote capture failed"));
              return;
            }
            qInfo() << "Native poll blocked PASS: durable unresolved "
                       "projection disables voting without dispatch";
            timer->stop();
            appPtr->exit(0);
            return;
          }
          auto *dialog = named(window, QStringLiteral("pollVoteDialog"));
          if (state->stage == 0) {
            if (!keyboardActivate(
                    named(pollBubble, QStringLiteral("pollVoteButton")))) {
              fail(QStringLiteral(
                  "explicit capability-gated Vote control is missing"));
              return;
            }
            state->stage = 1;
          } else if (state->stage == 1 || state->stage == 4) {
            if (!dialog || !dialog->property("opened").toBool())
              return;
            if (fixture->voteCount() != 0 ||
                !keyboardActivate(
                    named(dialog, QStringLiteral("pollVoteChoice1")))) {
              fail(QStringLiteral(
                  "selecting a poll option must not send a vote"));
              return;
            }
            state->stage = state->stage == 1 ? 2 : 5;
          } else if (state->stage == 2 || state->stage == 5) {
            auto *choiceTwo = named(dialog, QStringLiteral("pollVoteChoice2"));
            if (fixture->voteCount() != 0 || !choiceTwo ||
                choiceTwo->property("enabled").toBool() ||
                !keyboardActivate(
                    named(dialog, QStringLiteral("pollVoteReview")))) {
              fail(QStringLiteral(
                  "review vote must be explicit and must not send"));
              return;
            }
            state->stage = state->stage == 2 ? 3 : 6;
          } else if (state->stage == 3) {
            auto *confirm = named(dialog, QStringLiteral("pollVoteConfirm"));
            if (!confirm || !confirm->property("visible").toBool())
              return;
            if (fixture->voteCount() != 0 ||
                !keyboardActivate(
                    named(dialog, QStringLiteral("pollVoteCancel")))) {
              fail(QStringLiteral("canceling review sent a vote"));
              return;
            }
            state->stage = 7;
          } else if (state->stage == 7) {
            if (dialog && dialog->property("visible").toBool())
              return;
            if (fixture->voteCount() != 0 ||
                !keyboardActivate(
                    named(pollBubble, QStringLiteral("pollVoteButton")))) {
              fail(QStringLiteral(
                  "vote dialog did not reopen without dispatch"));
              return;
            }
            state->stage = 4;
          } else if (state->stage == 6) {
            auto *confirm = named(dialog, QStringLiteral("pollVoteConfirm"));
            if (!confirm || !confirm->property("visible").toBool())
              return;
            auto *review =
                named(dialog, QStringLiteral("pollVoteReviewChoices"));
            auto *confirmItem = qobject_cast<QQuickItem *>(confirm);
            if (!review || review->property("text").toString() != OptionOne ||
                review->property("textFormat").toInt() != 0 || !confirmItem ||
                confirmItem->mapToScene(QPointF(0, 0)).y() < 0 ||
                confirmItem
                        ->mapToScene(QPointF(confirmItem->width(),
                                             confirmItem->height()))
                        .y() > window->height()) {
              fail(QStringLiteral("vote review must show literal choices with "
                                  "an in-window confirm control"));
              return;
            }
            const auto capturePath =
                qEnvironmentVariable("WHATSFEATHER_POLL_TEST_CAPTURE");
            if (!capturePath.isEmpty() &&
                !window->grabWindow().save(capturePath + ".review.png")) {
              fail(QStringLiteral("review capture failed"));
              return;
            }
            if (!keyboardActivate(confirm)) {
              fail(QStringLiteral("confirm vote not keyboard actionable"));
              return;
            }
            state->stage = 8;
          } else if (state->stage == 8) {
            if (fixture->voteCount() != 1)
              return;
            auto *surface =
                named(pollBubble, QStringLiteral("pollResultsCardSurface"));
            auto *count =
                named(pollBubble, QStringLiteral("pollResultsOptionCount1"));
            if (!surface || !surface->property("visible").toBool() || !count ||
                count->property("text").toString() != "1 vote")
              return;
            if (fixture->unexpectedMethodCount() != 0 ||
                (dialog && dialog->property("visible").toBool())) {
              fail(QStringLiteral(
                  "vote dispatched wrong payload or left review open"));
              return;
            }
            const auto capturePath =
                qEnvironmentVariable("WHATSFEATHER_POLL_TEST_CAPTURE");
            if (!capturePath.isEmpty() &&
                !window->grabWindow().save(capturePath)) {
              fail(QStringLiteral("voting capture failed"));
              return;
            }
            qInfo()
                << "Native poll voting PASS: keyboard selection/review/cancel "
                   "sends nothing; explicit confirmation sends exactly once; "
                   "authoritative original-poll aggregate renders";
            timer->stop();
            appPtr->exit(0);
          }
          return;
        }

        // Every delegate owns hidden typed-card surfaces. Resolve the exact
        // message identity before looking up labels, not the first global name.
        auto *pollBubble = bubbleForMessage(window, MessageID);
        auto *resultsBubble = bubbleForMessage(window, ResultsMessageID);
        if (!pollBubble || !resultsBubble)
          return; // ListView instantiation is asynchronous; bounded by ticks.
        const auto resultRecord = resultsBubble->property("record").toMap();
        if (resultRecord.value("poll_results")
                .toMap()
                .value("question")
                .toString() != ResultsQuestion) {
          fail(QStringLiteral("poll results projection was lost at the "
                              "model/delegate boundary"));
          return;
        }
        auto *surface = named(pollBubble, QStringLiteral("pollCardSurface"));
        auto *question = named(pollBubble, QStringLiteral("pollQuestion"));
        auto *first = named(pollBubble, QStringLiteral("pollOptionLabel1"));
        auto *count = named(pollBubble, QStringLiteral("pollSelectableCount"));
        auto *body = named(pollBubble, QStringLiteral("messageBody"));
        auto *legacyBody =
            namedWithText(window, QStringLiteral("messageBody"), LegacyBody);
        auto *resultsSurface =
            named(resultsBubble, QStringLiteral("pollResultsCardSurface"));
        auto *resultsType =
            named(resultsBubble, QStringLiteral("pollResultsTypeLabel"));
        auto *resultsQuestion =
            named(resultsBubble, QStringLiteral("pollResultsQuestion"));
        auto *resultsFirst =
            named(resultsBubble, QStringLiteral("pollResultsOptionLabel1"));
        auto *resultsFirstCount =
            named(resultsBubble, QStringLiteral("pollResultsOptionCount1"));
        auto *resultsSecond =
            named(resultsBubble, QStringLiteral("pollResultsOptionLabel2"));
        auto *resultsSecondCount =
            named(resultsBubble, QStringLiteral("pollResultsOptionCount2"));
        auto *resultBody = named(resultsBubble, QStringLiteral("messageBody"));
        if (!surface || !surface->property("visible").toBool() || !question ||
            !first || !count || !body || !legacyBody || !resultsSurface ||
            !resultsSurface->property("visible").toBool() || !resultsType ||
            !resultsQuestion || !resultsFirst || !resultsFirstCount ||
            !resultsSecond || !resultsSecondCount) {
          fail(QStringLiteral("typed poll card or one of its semantic labels "
                              "was not rendered (surface=%1 visible=%2 "
                              "question=%3 option=%4 count=%5 body=%6)")
                   .arg(surface != nullptr)
                   .arg(surface ? surface->property("visible").toBool() : false)
                   .arg(question != nullptr)
                   .arg(first != nullptr)
                   .arg(count != nullptr)
                   .arg(body != nullptr));
          return;
        }
        auto *second = named(pollBubble, QStringLiteral("pollOptionLabel2"));
        if (question->property("text").toString() != Question ||
            first->property("text").toString() != OptionOne || !second ||
            second->property("text").toString() != OptionTwo ||
            count->property("text").toString() !=
                QStringLiteral("Select up to 1 option") ||
            body->property("visible").toBool() ||
            legacyBody->property("textFormat").toInt() != 0 ||
            legacyBody->property("text").toString() != LegacyBody ||
            resultsType->property("text").toString() !=
                QStringLiteral("Poll results · read-only") ||
            resultsQuestion->property("text").toString() != ResultsQuestion ||
            resultsFirst->property("text").toString() != ResultsOptionOne ||
            resultsFirstCount->property("text").toString() !=
                QStringLiteral("2 votes") ||
            resultsSecond->property("text").toString() != ResultsOptionTwo ||
            resultsSecondCount->property("text").toString() !=
                QStringLiteral("0 votes") ||
            (resultBody && resultBody->property("visible").toBool())) {
          fail(QStringLiteral("poll question/options/count or legacy-body "
                              "suppression changed"));
          return;
        }
        for (auto *field : {question, first, second, count, resultsType,
                            resultsQuestion, resultsFirst, resultsFirstCount,
                            resultsSecond, resultsSecondCount}) {
          if (field->property("textFormat").toInt() != 0) {
            fail(QStringLiteral("poll content must use Text.PlainText"));
            return;
          }
        }
        const auto checkAccessible = [&fail](QObject *object,
                                             const QString &expected) {
          auto *interface = QAccessible::queryAccessibleInterface(object);
          return interface && interface->text(QAccessible::Name) == expected;
        };
        if (!checkAccessible(question,
                             QStringLiteral("Poll question: ") + Question) ||
            !checkAccessible(first, QStringLiteral("Option 1: ") + OptionOne) ||
            !checkAccessible(second,
                             QStringLiteral("Option 2: ") + OptionTwo) ||
            !checkAccessible(count,
                             QStringLiteral("Selectable options count: 1")) ||
            !checkAccessible(resultsType,
                             QStringLiteral("Poll results · read-only")) ||
            !checkAccessible(resultsQuestion,
                             QStringLiteral("Poll results question: ") +
                                 ResultsQuestion) ||
            !checkAccessible(resultsFirst,
                             QStringLiteral("Option 1: ") + ResultsOptionOne) ||
            !checkAccessible(resultsFirstCount,
                             QStringLiteral("Vote count: 2")) ||
            !checkAccessible(resultsSecond,
                             QStringLiteral("Option 2: ") + ResultsOptionTwo) ||
            !checkAccessible(resultsSecondCount,
                             QStringLiteral("Vote count: 0"))) {
          fail(QStringLiteral(
              "poll card labels do not expose the expected accessible names"));
          return;
        }
        if (named(window, QStringLiteral("pollVoteButton")) ||
            named(window, QStringLiteral("pollResultsVoteButton")) ||
            fixture->unexpectedMethodCount() != 0) {
          fail(QStringLiteral(
              "read-only poll cards offered or dispatched a remote action"));
          return;
        }
        const auto capturePath =
            qEnvironmentVariable("WHATSFEATHER_POLL_TEST_CAPTURE");
        if (!capturePath.isEmpty()) {
          const auto capture = window->grabWindow();
          if (capture.isNull() || !capture.save(capturePath)) {
            fail(QStringLiteral("could not capture the synthetic poll card"));
            return;
          }
        }
        qInfo() << "Native poll results PASS: synthetic poll creation and "
                   "aggregate results use PlainText/accessibility; cards are "
                   "inert; no vote RPC";
        timer->stop();
        appPtr->exit(0);
      });
  timer->start();
}

void runPollDaemonTest(QGuiApplication &app, QQuickWindow *window,
                       Controller &controller) {
  if (!window) {
    qCritical() << "Native Go-daemon poll UI FAIL: missing window";
    app.exit(1);
    return;
  }
  struct State {
    int ticks = 0;
    bool selectionRequested = false;
  };
  auto state = std::make_shared<State>();
  auto *timer = new QTimer(&app);
  timer->setInterval(50);
  QObject::connect(
      timer, &QTimer::timeout, &app,
      [state, timer, window, controllerPtr = &controller, appPtr = &app] {
        auto fail = [timer, appPtr](const QString &message) {
          qCritical().noquote() << "Native Go-daemon poll UI FAIL:" << message;
          timer->stop();
          appPtr->exit(1);
        };
        auto &controller = *controllerPtr;
        if (++state->ticks > 240) {
          fail(QStringLiteral(
              "timed out waiting for real demo-daemon poll results"));
          return;
        }
        if (!controller.demo()) {
          fail(QStringLiteral("Controller is not in explicit demo mode"));
          return;
        }
        if (!controller.connected() || !controller.capabilitiesReady())
          return;
        const QString demoChatID = QStringLiteral("demo:alex");
        if (controller.selectedId() != demoChatID) {
          if (!state->selectionRequested &&
              controller.chats()->indexOf(demoChatID) >= 0) {
            state->selectionRequested = true;
            controller.selectChat(demoChatID);
          }
          return;
        }
        if (controller.loading())
          return;
        const QString messageID =
            QStringLiteral("fixture-go-daemon-poll-results");
        const int row = controller.messages()->indexOf(messageID);
        if (row < 0)
          return;
        const auto record = controller.messages()->row(row);
        if (record.value(QStringLiteral("kind")).toString() !=
                QStringLiteral("poll_results") ||
            record.value(QStringLiteral("chat_id")).toString() != demoChatID) {
          fail(QStringLiteral(
              "RPC model returned the fixture under the wrong chat or kind"));
          return;
        }
        auto *bubble = bubbleForMessage(window, messageID);
        if (!bubble)
          return;
        auto *surface = named(bubble, QStringLiteral("pollResultsCardSurface"));
        auto *type = named(bubble, QStringLiteral("pollResultsTypeLabel"));
        auto *question = named(bubble, QStringLiteral("pollResultsQuestion"));
        auto *first = named(bubble, QStringLiteral("pollResultsOptionLabel1"));
        auto *firstCount =
            named(bubble, QStringLiteral("pollResultsOptionCount1"));
        auto *second = named(bubble, QStringLiteral("pollResultsOptionLabel2"));
        auto *secondCount =
            named(bubble, QStringLiteral("pollResultsOptionCount2"));
        if (!surface || !surface->property("visible").toBool() || !type ||
            !question || !first || !firstCount || !second || !secondCount) {
          fail(QStringLiteral(
              "Go-daemon poll results did not render all typed QML fields"));
          return;
        }
        if (type->property("text").toString() !=
                QStringLiteral("Poll results · read-only") ||
            question->property("text").toString() != ResultsQuestion ||
            first->property("text").toString() != ResultsOptionOne ||
            firstCount->property("text").toString() !=
                QStringLiteral("2 votes") ||
            second->property("text").toString() != ResultsOptionTwo ||
            secondCount->property("text").toString() !=
                QStringLiteral("0 votes") ||
            named(bubble, QStringLiteral("pollVoteButton"))) {
          fail(QStringLiteral(
              "Go-daemon poll result values or read-only affordance changed"));
          return;
        }
        for (auto *field :
             {type, question, first, firstCount, second, secondCount}) {
          if (field->property("textFormat").toInt() != 0) {
            fail(QStringLiteral(
                "Go-daemon poll results must render as PlainText"));
            return;
          }
        }
        const auto checkAccessible = [](QObject *object,
                                        const QString &expected) {
          auto *interface = QAccessible::queryAccessibleInterface(object);
          return interface && interface->text(QAccessible::Name) == expected;
        };
        if (!checkAccessible(type,
                             QStringLiteral("Poll results · read-only")) ||
            !checkAccessible(question,
                             QStringLiteral("Poll results question: ") +
                                 ResultsQuestion) ||
            !checkAccessible(first,
                             QStringLiteral("Option 1: ") + ResultsOptionOne) ||
            !checkAccessible(firstCount, QStringLiteral("Vote count: 2")) ||
            !checkAccessible(second,
                             QStringLiteral("Option 2: ") + ResultsOptionTwo) ||
            !checkAccessible(secondCount, QStringLiteral("Vote count: 0"))) {
          fail(QStringLiteral(
              "Go-daemon poll results lack their expected accessible names"));
          return;
        }
        const auto capturePath =
            qEnvironmentVariable("WHATSFEATHER_POLL_TEST_CAPTURE");
        if (!capturePath.isEmpty()) {
          const auto capture = window->grabWindow();
          if (capture.isNull() || !capture.save(capturePath)) {
            fail(QStringLiteral(
                "could not capture real Go-daemon poll results"));
            return;
          }
        }
        qInfo() << "Native Go-daemon poll UI PASS: actual demo RPC → "
                   "Controller/model → QML read-only results";
        timer->stop();
        appPtr->exit(0);
      });
  timer->start();
}
