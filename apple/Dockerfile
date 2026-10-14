FROM python:3.14-slim
WORKDIR /app
COPY server ./server
COPY web ./web
RUN useradd --uid 10001 --create-home gryvia
USER 10001
EXPOSE 8443
ENTRYPOINT ["python3","-m","server.app"]
CMD ["--config","/run/gryvia/config.json","--db","/data/gryvia.db","--host","0.0.0.0","--cert","/run/gryvia/tls.crt","--key","/run/gryvia/tls.key"]
