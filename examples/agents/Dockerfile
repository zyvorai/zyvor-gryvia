# Reference GryviaAgent runtime (examples/agents/runtime.py).
FROM python:3.12-alpine
ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY runtime.py .
USER 65534
EXPOSE 8080
CMD ["python3", "/app/runtime.py"]
