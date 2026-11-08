FROM python:3.11-slim AS dependencies
WORKDIR /app
COPY requirements.txt .
RUN python -m venv /opt/mock-api \
    && /opt/mock-api/bin/pip install --no-cache-dir -r requirements.txt \
    && /opt/mock-api/bin/pip check \
    && /opt/mock-api/bin/pip uninstall -y pip setuptools wheel

FROM python:3.11-slim
RUN apt-get update && apt-get upgrade -y \
    && rm -rf /var/lib/apt/lists/* \
    && python -m pip uninstall -y pip setuptools wheel
COPY --from=dependencies /opt/mock-api /opt/mock-api
ENV PATH="/opt/mock-api/bin:${PATH}"
WORKDIR /app
COPY server.py .
EXPOSE 80
CMD ["uvicorn", "server:app", "--host", "0.0.0.0", "--port", "80"]
