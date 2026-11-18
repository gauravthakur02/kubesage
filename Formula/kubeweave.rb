# Homebrew formula: builds kubeweave from source on your Mac, so macOS never shows the
# "Apple could not verify" warning (that only happens to binaries downloaded in a browser).
#
#   brew tap gauravthakur02/kubeweave https://github.com/gauravthakur02/kubeweave
#   brew trust gauravthakur02/kubeweave   # Homebrew 7+: trust the tap once
#   brew install kubeweave
#
# The release workflow updates url and sha256 for every new tag.
class Kubeweave < Formula
  desc "Kubernetes IDE for your terminal with an AI copilot"
  homepage "https://github.com/gauravthakur02/kubeweave"
  url "https://github.com/gauravthakur02/kubeweave/archive/refs/tags/v0.7.0.tar.gz"
  sha256 "1e54b94487dba3c96521cae462596839e5dd4dcddf0143553a7a0f8b97b7f2d6"
  license "MIT"
  head "https://github.com/gauravthakur02/kubeweave.git", branch: "main"

  depends_on "go" => :build

  def install
    ENV["CGO_ENABLED"] = "0"
    system "go", "build", *std_go_args(ldflags: "-s -w -X main.version=#{version}")
    if version >= Version.new("0.3.0") || build.head?
      generate_completions_from_executable(bin/"kubeweave", "completion", shells: [:bash, :zsh, :fish])
      (man1/"kubeweave.1").write Utils.safe_popen_read(bin/"kubeweave", "man")
    end
  end

  def caveats
    <<~TEXT
      Start with: kubeweave   (add --readonly to disable every change)
      The first start shows a one-minute tour; :tour shows it again.
    TEXT
  end

  test do
    ENV["KUBEWEAVE_NO_UPDATE_CHECK"] = "1"
    ENV["NO_COLOR"] = "1"
    assert_match version.to_s, shell_output("#{bin}/kubeweave version")
  end
end
