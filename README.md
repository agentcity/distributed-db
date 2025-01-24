# Распределенная база данных с использованием Raft

Описание проекта:

Этот проект представляет собой реализацию распределенной базы данных, которая использует консенсусный алгоритм Raft для обеспечения согласованности данных между узлами. Проект включает в себя следующие основные компоненты:

1. Узлы базы данных (Nodes): Каждый узел хранит часть данных и участвует в процессе консенсуса.
2. Координатор (Coordinator): Координатор отвечает за балансировку нагрузки между узлами и предоставляет единую точку входа для клиентских запросов.
3. gRPC API: Для взаимодействия между узлами и координатором используется gRPC.
4. HTTP API: Узлы и координатор предоставляют HTTP API для мониторинга, управления и взаимодействия.
5. Prometheus: Для сбора метрик используется Prometheus.
6. Swagger UI: Документация к HTTP API доступна через Swagger UI.
7. TLS: Поддерживается TLS для безопасного взаимодействия.
8. Аутентификация: HTTP API защищен базовой аутентификацией.
9. Rate Limiter: Для защиты от перегрузки на узлах используется rate limiter.

Архитектура:

1. Узлы (Nodes):
  •  Используют алгоритм Raft для согласования данных.
  •  Хранят данные в in-memory хэш-таблице.
  •  Имеют роли: Лидер (Leader), Кандидат (Candidate), Последователь (Follower).
  •  Обмениваются сообщениями через gRPC.
  •  Предоставляют HTTP API для записи (PUT) и чтения (GET) данных.
  •  Предоставляют HTTP API для удаления данных (DELETE).
  •  Отправляют метрики (загрузку) координатору.
  •  Используют таймер выборов для избрания лидера.
  •  Используют heartbeat для обнаружения сбоев.
2. Координатор (Coordinator):
  •  Получает отчеты о загрузке от узлов.
  •  Определяет наименее загруженный узел для выполнения запросов.
  •  Предоставляет единую точку входа для клиентских запросов.
  •  Предоставляет HTTP API для удаления данных (DELETE).
 *  Предоставляет HTTP API для голосования (VOTE)
  •  Использует пул gRPC соединений для взаимодействия с узлами.
3. gRPC:
  •  Определения сервисов (.proto) для взаимодействия между узлами и координатором.
4. HTTP API:
  •  Для мониторинга, управления и взаимодействия с узлами и координатором.
  •  Включает endpoints для put/get/delete данных и healthcheck.
  •  Защищен базовой аутентификацией.
5. Prometheus:
  •  Собирает метрики (количество запросов put, get и ошибок).
  •  Используется для мониторинга системы.
6. Swagger UI:
  •  Генерирует документацию для HTTP API.

Структура проекта:

```
distributed-db/
├── internal/
│  ├── auth/
│  │  └── auth.go
│  ├── data/
│  │  └── data.go
│  └── grpcpool/
│    └── pool.go
├── node/
│  ├── node.go    // Основная структура Node, NewNode, StartServer, логирование
│  ├── http/
│  │  └── http.go  // Обработка HTTP запросов
│  ├── grpc/
│  │  └── grpc.go  // Обработка gRPC запросов
│  ├── raft/
│  │  └── raft.go  // Логика Raft
│  └── metrics/
│    └── metrics.go  // Сбор и отправка метрик
├── proto/
│  └── db.proto   // Определение proto файла
├── docs/       // Каталог для Swagger UI
│  └── swagger.json
├── main.go
├── go.mod
└── go.sum

```

Инструкция по запуску:

1. Установите Go: Убедитесь, что у вас установлен Go (версия 1.20 или выше).
2. Склонируйте репозиторий:
```bash
    git clone <ваш_репозиторий>
    cd distributed-db
```
3. Установите зависимости:
```bash
    go mod tidy
```
4. Сгенерируйте gRPC код:
```bash
    protoc --go_out=. --go-grpc_out=. proto/node.proto
```
5. Сгенерируйте Swagger JSON:
```bash
    swag init -g cmd/main.go
```
6. Создайте TLS сертификаты (опционально): Если вы планируете использовать TLS, создайте необходимые сертификаты. Можно использовать следующую команду для создания тестовых сертификатов:
```bash
    openssl req -x509 -newkey rsa:4096 -keyout certs/server.key -out certs/server.crt -days 365 -nodes -subj "/CN=localhost"
    openssl req -newkey rsa:4096 -keyout certs/client.key -out certs/client.crt -days 365 -nodes -subj "/CN=localhost"
```
Внимание: Эти сертификаты предназначены только для тестирования. В продакшене используйте действительные сертификаты.

7. Запустите узлы и координатор: Запускайте узлы и координатор в разных терминалах или в разных окнах терминала.
   
  •  Запуск Координатора:
```bash
    go run cmd/main.go -id=0 -address=":8081" -logLevel="DEBUG" -tls=true
```
  •  Запуск Узлов:  
```bash
    go run cmd/main.go -id=1 -address=":8082" -otherNodes=":8083,:8084" -shardId=1 -coordinatorAddress=":8081" -logLevel="DEBUG" -tls=true
    go run cmd/main.go -id=2 -address=":8083" -otherNodes=":8082,:8084" -shardId=1 -coordinatorAddress=":8081" -logLevel="DEBUG" -tls=true
    go run cmd/main.go -id=3 -address=":8084" -otherNodes=":8082,:8083" -shardId=1 -coordinatorAddress=":8081" -logLevel="DEBUG" -tls=true
```
*  Параметры:
    *  -id: Идентификатор узла (0 для координатора, 1 и более для узлов).
    *  -address: Адрес, на котором будет слушать узел.
    *  -otherNodes: Список адресов других узлов через запятую.
    *  -shardId: Идентификатор шарда для узла.
    *  -coordinatorAddress: Адрес координатора.
    *  -logLevel: Уровень логирования (DEBUG, INFO, ERROR).
    *  -tls: Включить TLS (true или false).
    *  -serverCert: Путь к серверному сертификату.
    *  -clientCert: Путь к клиентскому сертификату.
    *  -authUser: Имя пользователя для базовой аутентификации.
    *  -authPass: Пароль для базовой аутентификации.
    *  -authUser и -authPass по умолчанию admin/admin, при запуске можете поменять на свои

8. Используйте HTTP API:
  •  Swagger UI: Откройте http://localhost:8081/swagger/ в браузере, чтобы увидеть документацию HTTP API для координатора
  •  Запись данных (PUT): Отправьте PUT запрос на узел (например, http://localhost:8082/put?key=test).
  •  Чтение данных (GET): Отправьте GET запрос на узел (например, http://localhost:8082/get?key=test).
  •  Удаление данных (DELETE): Отправьте DELETE запрос на узел или координатор (например, http://localhost:8082/delete/test or http://localhost:8081/delete/test).

   Запросы PUT, GET, DELETE на узлах необходимо аутентифицировать с помощью Basic Auth.

   Пример команды cURL с базовой аутентификацией:
   
```bash  
    curl -u admin:admin -X PUT "http://localhost:8082/put?key=test"
```
```bash  
    curl -u admin:admin -X GET "http://localhost:8082/get?key=test"
```
```bash  
    curl -u admin:admin -X GET "http://localhost:8082/get?key=test"
```

Пример использования:

1. Запустите координатор и три узла, как описано в инструкции.
2. Отправьте PUT запрос на один из узлов:
```bash  
    curl -u admin:admin -X PUT "http://localhost:8082/put?key=testKey"
```  
3. Отправьте GET запрос на любой узел и убедитесь, что значение было записано:
```bash  
    curl -u admin:admin -X GET "http://localhost:8083/get?key=testKey"
```
Узлы будут перенаправлять запросы PUT и GET на текущего лидера шарда.
4. Удалите данные через HTTP API одного из узлов
```bash  
    curl -u admin:admin -X DELETE "http://localhost:8082/delete/testKey"
```
5.  Отправьте GET запрос на любой узел и убедитесь, что значение удалено
```bash  
    curl -u admin:admin -X GET "http://localhost:8082/get?key=testKey"
```
6. Удалите данные через HTTP API координатора
```bash  
    curl -u admin:admin -X DELETE "http://localhost:8081/delete/testKey"
```
7.  Отправьте GET запрос на любой узел и убедитесь, что значение удалено
```bash  
    curl -u admin:admin -X GET "http://localhost:8082/get?key=testKey"
```

Возможные улучшения:

1. Персистентность данных: В текущей реализации данные хранятся в памяти. Необходимо добавить сохранение данных на диск для обеспечения персистентности.
2. Более продвинутая балансировка нагрузки: Текущая балансировка использует наименее загруженный узел. Можно использовать более продвинутые алгоритмы балансировки.
3. Улучшение Fault Tolerance: Добавить механизмы обнаружения и восстановления после сбоев узлов.
4. Скейлинг: Продумать механизм масштабирования системы при увеличении количества узлов и данных.
5. Более сложные запросы: Добавить поддержку более сложных запросов к базе данных, таких как выборка по условию.
6. Улучшенное логирование: Использовать более продвинутые инструменты логирования.

Технические решения:

1. Raft: Реализация Raft в соответствии со статьей, с выборами лидера, передачей логов и heartbeat.
2. gRPC: Для взаимодействия между узлами и координатором, так как это бинарный протокол, который более производителен чем REST.
3. Хэш-таблица: In-memory хэш-таблица используется для хранения данных для простоты и скорости доступа к данным.
4. Базовая аутентификация: Для обеспечения безопасности HTTP API.
5. Rate Limiting: Ограничивает количество запросов для предотвращения перегрузки узлов.
6. Пул соединений: Для эффективного управления gRPC соединениями.

Заключение:

Этот проект представляет собой базовую реализацию распределенной базы данных с использованием Raft. Он может быть использован в качестве основы для дальнейших исследований и разработок в области распределенных систем.

Дополнительные замечания:

•  Пожалуйста, обратите внимание, что этот проект является демонстрационным и может потребовать дополнительных доработок для использования в производственной среде.
•  Для корректной работы алгоритма Raft необходимо запускать не менее 3-х узлов.

