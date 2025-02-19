schema = "1"

project "consul" {
  team = "consul core"
  slack {
    # feed-consul-ci
    notification_channel = "C9KPKPKRN"
  }
  github {
    organization = "hashicorp"
    repository = "consul"
    release_branches = [
      "main",
      "release/**",
    ]
  }
}

event "build" {
  action "build" {
    organization = "hashicorp"
    repository = "crt-workflows-common"
    workflow = "build"
  }
}

event "prepare" {
  depends = ["build"]
  action "prepare" {
    organization = "hashicorp"
    repository   = "crt-workflows-common"
    workflow     = "prepare"
    depends      = ["build"]
  }

  notification {
    on = "fail"
  }
}

event "trigger-production" {
// This event is dispatched by the bob trigger-promotion command
// and is required - do not delete.
}

event "promote-production" {
  depends = ["trigger-production"]
  action "promote-production" {
    organization = "hashicorp"
    repository = "crt-workflows-common"
    workflow = "promote-production"
  }

  notification {
    on = "always"
  }
}

event "promote-production-docker" {
  depends = ["promote-production"]
  action "promote-production-docker" {
    organization = "hashicorp"
    repository = "crt-workflows-common"
    workflow = "promote-production-docker"
  }

  notification {
    on = "always"
  }
}

event "promote-production-packaging" {
  depends = ["promote-production-docker"]
  action "promote-production-packaging" {
    organization = "hashicorp"
    repository = "crt-workflows-common"
    workflow = "promote-production-packaging"
  }

  notification {
    on = "always"
  }
}

event "post-publish-website" {
  depends = ["promote-production-packaging"]
  action "post-publish-website" {
    organization = "hashicorp"
    repository = "crt-workflows-common"
    workflow = "post-publish-website"
  }

  notification {
    on = "always"
  }
}
event "bump-version" {
  depends = ["post-publish-website"]
  action "bump-version" {
    organization = "hashicorp"
    repository = "crt-workflows-common"
    workflow = "bump-version"
  }

  notification {
    on = "fail"
  }
}

event "update-ironbank" {
  depends = ["bump-version"]
  action "update-ironbank" {
    organization = "hashicorp"
    repository = "crt-workflows-common"
    workflow = "update-ironbank"
  }

  notification {
    on = "fail"
  }
}
