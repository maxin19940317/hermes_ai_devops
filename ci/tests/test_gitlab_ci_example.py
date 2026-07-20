"""Static integration contract for the GitLab 13.8 CI example."""
from pathlib import Path


EXAMPLE = Path(__file__).parents[1] / "gitlab-ci.example.yml"


def test_example_passes_deterministic_identity_inputs():
    text = EXAMPLE.read_text(encoding="utf-8")

    source_date_epoch = (
        'export SOURCE_DATE_EPOCH="$(date --date="${CI_COMMIT_TIMESTAMP}" +%s)"'
    )
    assert source_date_epoch in text
    assert text.index(source_date_epoch) < text.index("./release_pack.sh")
    assert '--source-date-epoch "${SOURCE_DATE_EPOCH}"' in text
    assert '--pipeline-global-id "${CI_PIPELINE_ID}"' in text
    assert '--created-at "${CI_COMMIT_TIMESTAMP}"' in text
    assert "CI_PIPELINE_CREATED_AT" not in text

    bundle_name = (
        'BUNDLE_FILE="bundle-g${CI_COMMIT_SHORT_SHA}-p${CI_PIPELINE_ID}.json"'
    )
    assert bundle_name in text
    assert '--upload-file "dist/${BUNDLE_FILE}"' in text
