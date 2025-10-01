#!/bin/bash

# run.sh - A comprehensive script to manage the CLIProxyAPI application lifecycle.
# This script provides a unified interface for starting, stopping, restarting,
# and checking the status of the application using docker-compose.

# --- Configuration ---
COMPOSE_FILE="docker-compose.yml"
SERVICE_NAME="cli-proxy-api"

# --- Helper Functions ---

# Function to check if docker-compose is available
check_docker_compose() {
    if ! command -v docker-compose &> /dev/null; then
        echo "Error: docker-compose is not installed. Please install it to continue."
        exit 1
    fi
}

# Function to check the status of the service
check_status() {
    if docker-compose -f "$COMPOSE_FILE" ps -q "$SERVICE_NAME" | grep -q .; then
        echo "Service is running."
        return 0
    else
        echo "Service is not running."
        return 1
    fi
}

# Function to start the application
start_app() {
    if check_status > /dev/null; then
        echo "Application is already running."
        return
    fi

    echo "Starting the application..."
    docker-compose -f "$COMPOSE_FILE" up -d --remove-orphans
    if [ $? -eq 0 ]; then
        echo "Application started successfully."
    else
        echo "Failed to start the application."
    fi
}

# Function to stop the application
stop_app() {
    if ! check_status > /dev/null; then
        echo "Application is not running."
        return
    fi

    echo "Stopping the application..."
    docker-compose -f "$COMPOSE_FILE" down
    if [ $? -eq 0 ]; then
        echo "Application stopped successfully."
    else
        echo "Failed to stop the application."
    fi
}

# Function to restart the application
restart_app() {
    echo "Restarting the application..."
    stop_app
    sleep 2
    start_app
}

# --- Main Script ---

# Ensure docker-compose is available
check_docker_compose

# Parse command-line arguments
case "$1" in
    start)
        start_app
        ;;
    stop)
        stop_app
        ;;
    restart)
        restart_app
        ;;
    status)
        check_status
        ;;
    *)
        echo "Usage: $0 {start|stop|restart|status}"
        exit 1
        ;;
esac