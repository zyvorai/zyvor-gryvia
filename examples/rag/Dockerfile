# Reference ingestion image of GryviaVectorIndex (the ai-operator's --rag-ingest-image default,
# ghcr.io/zyvorai/gryvia-rag-ingest). ingest.py only uses the Python standard library.
FROM python:3.12-alpine
WORKDIR /app
COPY ingest.py /app/ingest.py
USER 65534:65534
ENTRYPOINT ["python3", "-u", "/app/ingest.py"]
