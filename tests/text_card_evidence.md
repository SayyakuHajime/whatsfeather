# Synthetic text card evidence

All data in these checks is local and synthetic. No WhatsApp connection or live acceptance was attempted. The canonical verifier was not run. Native rendering and captures need a host run because this sandbox rejects Unix socket binding.

## Changes

- `backend/internal/app/app.go`
- `backend/internal/app/text_card.go`
- `backend/internal/app/text_card_test.go`
- `backend/internal/transport/whatsapp/client.go`
- `backend/internal/transport/whatsapp/text_card.go`
- `backend/internal/transport/whatsapp/text_card_test.go`
- `desktop/qml/MessageBubble.qml`
- `desktop/src/main.cpp`
- `desktop/src/textcardtest.cpp`
- `tests/test_text_card.py`
- `tests/integration/test_text_cards.py`
- `tests/text_card_evidence.md`
- `CMakeLists.txt`
- `scripts/verify.sh`

Existing tracked and untracked work was kept. No branch, worktree, staging, or commit was made.

## RED to GREEN checks

Go commands below ran from `backend/`. Each RED test was added and run before the production change for that behavior. Later table cases extend coverage; they do not have separate RED runs for each variant.

1. `GOPROXY=off GOSUMDB=off go test ./internal/app -run '^TestTextCardReopen$' -count=1`
   - RED: `missing typed text card field`.
   - GREEN: exit 0 after typed fields, transactional table, fallback, and bounded list hydration were added.
2. `GOPROXY=off GOSUMDB=off go test ./internal/app -run '^TestTextCardValidation$' -count=1`
   - RED: accepted an empty projection.
   - GREEN: `go test ./internal/app -run '^TestTextCard(Validation|Reopen)$' -count=1` with the same environment, exit 0 after string/count/size/kind validation.
3. `GOPROXY=off GOSUMDB=off go test ./internal/app -run '^TestTextCardStoredValidationAndCascade$' -count=1`
   - RED: accepted interactive stored JSON with labels.
   - GREEN: `go test ./internal/app -run '^TestTextCard' -count=1` with the same environment, exit 0 after stored kind/label checks.
4. `GOCACHE=/tmp/wf-card-go GOPROXY=off GOSUMDB=off go test ./internal/transport/whatsapp -run '^TestInteractiveTextCard$' -count=1`
   - RED: `lost interactive kind`, observed `kind:"text"`.
   - GREEN: exit 0 after the safe InteractiveMessage adapter was added.
5. `GOCACHE=/tmp/wf-card-go GOPROXY=off GOSUMDB=off go test ./internal/transport/whatsapp -run '^TestTemplateTextCard$' -count=1`
   - RED: `lost template kind`, observed `kind:"text"`.
   - GREEN: exit 0 after the hydrated/nested template adapter was added.
6. `GOCACHE=/tmp/wf-card-go GOPROXY=off GOSUMDB=off go test ./internal/transport/whatsapp -run '^TestTextCardRejectsUnknownButtonPayload$' -count=1`
   - RED: `unknown button payload accepted`.
   - GREEN: exit 0 after unknown hydrated button payload fields were rejected.
7. `GOCACHE=/tmp/wf-card-go GOPROXY=off GOSUMDB=off go test ./internal/app -run '^TestTextCardRejectsAmbiguousJSON$' -count=1`
   - RED: accepted duplicate `body` keys.
   - GREEN: `go test ./internal/app -run '^TestTextCard' -count=1` with the same environment, exit 0 after strict keys/types, duplicate checks, and UTF-16 surrogate checks.
8. From the repo root: `python3 tests/test_text_card.py`
   - RED: missing `textCardCardSurface` in QML.
   - GREEN: exit 0 after the PlainText card, static labels/names, and disclosure were added.

An early test-file write used a path relative to the wrong directory and wrote no file; its `no tests to run` result was not RED evidence. An adapter test first hit a read-only default Go cache; the command was rerun with the cache under `/tmp` as listed above.

## Further checks

- `GOCACHE=/tmp/wf-card-go GOPROXY=off GOSUMDB=off go test ./internal/transport/whatsapp -run '^TestTextCardsAdapterSQLiteRPC$' -count=1`: PASS. Adapter input, SQLite close/reopen, typed RPC fields, separate quote preview, and secret sentinel absence in RPC and the closed SQLite file.
- `GOCACHE=/tmp/wf-card-go GOPROXY=off GOSUMDB=off go test ./internal/transport/whatsapp -run '^Test(TextCard|InteractiveTextCard|TemplateTextCard)' -count=1`: PASS. Unsupported variants, nil wrappers, invalid text, counts, and safe containers.
- `GOCACHE=/tmp/wf-card-go GOPROXY=off GOSUMDB=off go test ./internal/app ./internal/transport/whatsapp -count=1`: PASS; app 118.396s, adapter 18.919s.
- `GOCACHE=/tmp/wf-card-go GOPROXY=off GOSUMDB=off go vet ./internal/app ./internal/transport/whatsapp`: PASS.
- `cmake -S . -B build -DCMAKE_BUILD_TYPE=Release`: PASS.
- `cmake --build build --parallel 2`: PASS, including QML compilation and the native fixture.
- `ctest --test-dir build -R '^text-card-source$' --output-on-failure`: PASS, 1/1.
- `python3 -m py_compile tests/test_text_card.py tests/integration/test_text_cards.py`: PASS.
- `bash -n scripts/verify.sh`: PASS. The verifier itself was not run.

## Native runtime gap and guards

`python3 tests/integration/test_text_cards.py` initially failed with `Unknown option 'text-card-test'`. After fixture registration and a build, it failed at `QLocalServer::listen: Unknown error 1`, before rendering. `ctest --test-dir build -R '^text-card-integration$' --output-on-failure` reported the same socket bind error (exit 8). This is a blocked runtime check, not a GREEN result.

A separate Python subprocess check used temporary XDG roots, `QT_QPA_PLATFORM=offscreen`, `QT_QUICK_BACKEND=software`, and `QT_FORCE_STDERR_LOGGING=1`. It invoked:

- `build/bin/whatsfeather --live --text-card-test --data-dir <temporary-profile>`
- `build/bin/whatsfeather --demo --text-card-test --socket <temporary-path>/sentinel.sock --data-dir <temporary-profile>`

Both exited 2, emitted the expected guard message, and left the profile absent. That check did not bind a sentinel listener. Its first attempt checked stderr without forcing Qt stderr logging and was corrected to use combined output and the logging flag.

The integration harness includes a real sentinel listener, same-UID private socket checks, both kinds, legacy text, PlainText/accessibility/disclosure checks, no button styling, zero unexpected RPCs, and dark/light captures in temporary paths. These assertions still require host execution; no captures or sentinel-listener result are claimed here.

## Unsupported content

Media headers/footers, attachment flags, Bloks, carousel, collection, shop, unknown protocol variants, malformed wrappers, legacy FourRowTemplate, missing/non-text hydrated titles, ambiguous dual template formats, and unknown/missing hydrated buttons stay unsupported. Native-flow names/params and template action metadata are ignored and never copied into the typed projection. All supported labels remain static text; no send, URL, call, vote, or other protocol action was added.
