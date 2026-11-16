---
task_id: "0003"
type: chore
title: Деплой, CI и документация
created_at: 2026-11-16T00:50:00Z
author: saron
scope:
  backend: true
  console: false
  mobile: false
---

# Деплой, CI и документация

## Проблема

Проект запускался только вручную, документация описывала несуществующие
endpoint'ы, Swagger JSON был битым, Docker/CI отсутствовали.

## Требования

- Dockerfile (multi-stage, non-root) и compose на 2 шарда × 3 реплики + координатор.
- Makefile: build/test/vet/proto/compose/smoke/dev.
- CI: `go vet`, `go test -race`, `go build`, docker build.
- README: архитектура, быстрый старт, API, флаги, гарантии и ограничения.
- OpenAPI 3.0 для координатора.
- Smoke-скрипт PUT/GET/DELETE через координатор.

## Критерии приёмки

- [ ] `docker build` собирает образ.
- [ ] `docker compose up -d` поднимает 6 узлов + координатор, все healthy.
- [ ] `scripts/smoke.sh` проходит против compose-кластера.
- [ ] README и OpenAPI соответствуют фактическому API.
- [ ] `make test` и CI-команды воспроизводимы.

## Ссылки

- `docker-compose.yml`, `Dockerfile`, `Makefile`
- `docs/openapi.yaml`, `README.md`
