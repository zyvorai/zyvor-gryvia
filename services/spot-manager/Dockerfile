# Build stage
FROM python:3.12-slim AS builder

WORKDIR /build

COPY requirements.txt .
RUN pip install --no-cache-dir --prefix=/install -r requirements.txt

COPY . .

# Runtime stage
FROM python:3.12-slim

RUN groupadd --gid 65532 nonroot && \
    useradd --uid 65532 --gid 65532 --shell /bin/false --create-home nonroot

WORKDIR /app

COPY --from=builder /install /usr/local
COPY --from=builder /build /app

EXPOSE 8080

USER 65532:65532

ENTRYPOINT ["python", "-m", "uvicorn", "main:app", "--host", "0.0.0.0", "--port", "8080"]
