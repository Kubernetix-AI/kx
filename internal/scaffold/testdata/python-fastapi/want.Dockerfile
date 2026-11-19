# Written by kx deploy for a Python app.
FROM python:3.13-slim
WORKDIR /app
COPY requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt
COPY . .
ENV PORT=8080 PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1
EXPOSE 8080
# Some hosts refuse containers that run as root, and can only check a numeric user.
RUN useradd --uid 10001 --create-home app
USER 10001
CMD ["uvicorn","main:app","--host","0.0.0.0","--port","8080"]
