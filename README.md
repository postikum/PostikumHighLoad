# PostikumHighload

PostikumHighload — учебный high-load сервис на Go для обработки потоковых данных с базовой аналитикой нагрузки и развёртыванием в Kubernetes.

Проект демонстрирует:
- обработку потоковых HTTP-запросов,
- простую статистическую аналитику (rolling average, z-score),
- использование Redis как кэша,
- мониторинг через Prometheus и Grafana,
- автоскейлинг в Kubernetes (HPA),

---

## HTTP эндпоинты

### POST /ingest
Принимает метрики:
```json
{
  "timestamp": 1700000000,
  "cpu": 0.42,
  "rps": 900
}
```

### GET /analyze
Возвращает результат аналитики:
```json
{
  "rolling_avg": 880.5,
  "z_score": 2.3,
  "anomaly": true
}
```

### GET /metrics
Экспорт метрик в формате Prometheus.

---

## Технологический стек

- Go 1.23
- Redis
- Docker
- Kubernetes (Minikube)
- Prometheus
- Grafana

---

## Структура репозитория

```
PostikumHighload/
├── main.go
├── go.mod
├── go.sum
├── Dockerfile
├── locustfile.py
├── k8s/
│   ├── app.yaml
│   ├── redis.yaml
│   ├── hpa.yaml
│   ├── ingress.yaml
│   ├── servicemonitor.yaml
└── README.md
```

---

## Сборка Docker-образа

```bash
docker build -t postikumhighload:1.0 .
```

---

## Запуск в Minikube

```bash
minikube start --driver=docker --cpus=2 --memory=4g
minikube addons enable ingress
minikube addons enable metrics-server
minikube image load postikumhighload:1.0
kubectl apply -f k8s/
```

## Автоскейлинг

HPA:
- minReplicas: 2
- maxReplicas: 4
- CPU target: 70%
