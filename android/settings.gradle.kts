// SPDX-License-Identifier: AGPL-3.0-or-later

pluginManagement {
    repositories {
        google {
            // Narrowed to Google's own groups. A build that can pull any
            // artifact from any repository is one where a typo'd coordinate
            // silently resolves somewhere else.
            content {
                includeGroupByRegex("com\\.android.*")
                includeGroupByRegex("com\\.google.*")
                includeGroupByRegex("androidx.*")
            }
        }
        mavenCentral()
        gradlePluginPortal()
    }
}

dependencyResolutionManagement {
    // Projects declare no repositories of their own, so every dependency in
    // this build is resolved from a list that is written down in one place -
    // which is the property an F-Droid reviewer has to be able to check.
    repositoriesMode.set(RepositoriesMode.FAIL_ON_PROJECT_REPOS)
    repositories {
        google {
            content {
                includeGroupByRegex("com\\.android.*")
                includeGroupByRegex("com\\.google.*")
                includeGroupByRegex("androidx.*")
            }
        }
        mavenCentral()
    }
}

rootProject.name = "lexarii"
include(":app")
