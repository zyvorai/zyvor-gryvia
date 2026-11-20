# One image for the download, fine-tune, quantize and evaluate steps of the model factory.
#   docker build -t <registry>/gryvia-model-factory:1.0 examples/model-factory
FROM pytorch/pytorch:2.8.0-cuda12.8-cudnn9-runtime

ENV PYTHONUNBUFFERED=1 \
    HF_HOME=/models/hf-home \
    HF_HUB_ENABLE_HF_TRANSFER=0

WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY outputs.py download.py finetune_lora.py quantize.py evaluate.py ./

USER 65534:65534
