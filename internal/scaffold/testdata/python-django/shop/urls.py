from django.http import HttpResponse
from django.urls import path


def index(request):
    return HttpResponse("python-django\n")


urlpatterns = [path("", index)]
