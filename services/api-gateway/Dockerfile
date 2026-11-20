FROM python:3.11-slim

WORKDIR /app

# Install dependencies
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# Copy application code
COPY main.py .
COPY routers/ routers/
COPY intelligence/ intelligence/

# Create non-root user
RUN useradd -m -u 1000 apigateway && \
    chown -R apigateway:apigateway /app

USER apigateway

EXPOSE 8080

# Health check
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD python -c "import os,ssl,urllib.request as u; tls=os.environ.get('GRYVIA_TLS_CERT'); u.urlopen(('https' if tls else 'http')+'://localhost:8080/health', context=ssl._create_unverified_context() if tls else None)"

# Serves HTTPS when GRYVIA_TLS_CERT/GRYVIA_TLS_KEY are set (the deployment mints a self-signed pair).
CMD ["sh", "-c", "exec uvicorn main:app --host 0.0.0.0 --port 8080 ${GRYVIA_TLS_CERT:+--ssl-certfile $GRYVIA_TLS_CERT --ssl-keyfile $GRYVIA_TLS_KEY}"]
