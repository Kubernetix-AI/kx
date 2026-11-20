# Written by kx deploy for a Node app.
FROM node:20-alpine
WORKDIR /app
COPY package.json ./
RUN npm install
COPY . .
ENV PORT=8080 HOST=0.0.0.0
# npm keeps its cache and logs in /tmp, so the app runs as any user.
ENV NPM_CONFIG_CACHE=/tmp/.npm NPM_CONFIG_UPDATE_NOTIFIER=false
EXPOSE 8080
# Some hosts refuse containers that run as root, and can only check a numeric user.
USER 1000
CMD ["node","server.js"]
