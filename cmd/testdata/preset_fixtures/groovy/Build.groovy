package com.example.fixture

import org.gradle.api.Project
import org.gradle.api.Plugin

plugins {
    id 'java'
    id 'groovy'
}

group = 'com.example'
version = '0.1.0'

class GreetingPlugin implements Plugin<Project> {
    void apply(Project project) {
        project.task('greet') {
            doLast {
                println "Hello from ${project.name}"
            }
        }
    }
}

class BuildConfig {
    String name
    String version
    List<String> dependencies = []

    String describe() {
        return "${name} v${version}"
    }
}

def configureRepositories(project) {
    project.repositories {
        mavenCentral()
        gradlePluginPortal()
    }
}

repositories {
    mavenCentral()
}

dependencies {
    implementation 'org.codehaus.groovy:groovy-all:3.0.21'
    testImplementation 'junit:junit:4.13.2'
}
