# Template for Formula/zstdir.rb in the arjkb/homebrew-tap repository.
# See RELEASING.md for how to update it for each release.
class Zstdir < Formula
  desc "Compress and extract directories with zstd in one step"
  homepage "https://github.com/arjkb/zstdir"
  url "https://github.com/arjkb/zstdir/archive/refs/tags/v0.1.0.tar.gz"
  sha256 "0000000000000000000000000000000000000000000000000000000000000000"
  license "GPL-3.0-only"
  head "https://github.com/arjkb/zstdir.git", branch: "main"

  depends_on "go" => :build

  def install
    system "go", "build", *std_go_args(ldflags: "-X main.version=#{version}")
    bin.install_symlink "zstdir" => "unzstdir"
    man1.install "man/zstdir.1"
    man1.install_symlink "zstdir.1" => "unzstdir.1"
  end

  test do
    (testpath/"proj/sub/a.txt").write "hello\n"
    system bin/"zstdir", "proj"
    assert_path_exists testpath/"proj.tar.zst"

    rm_r testpath/"proj"
    system bin/"unzstdir", "proj.tar.zst"
    assert_equal "hello\n", (testpath/"proj/sub/a.txt").read

    assert_match version.to_s, shell_output("#{bin}/zstdir --version")
  end
end
